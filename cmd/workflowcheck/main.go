// Command workflowcheck 通过真实 HTTP 路由执行本地业务链路检查。
//
// 每条工作流都在与生产入口相同的资源边界内运行：临时监听由
// internal/config 的服务配置构造（含读写与空闲超时、关闭期限），
// 就绪等待、探针并发度和请求超时来自探针配置，生命周期复用
// internal/server 的阶段模型。任何失败都绑定阶段标签并映射到固定
// 退出码，函数返回前监听与后台 goroutine 全部归零。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"example.com/paleo-prep-station/internal/config"
	"example.com/paleo-prep-station/internal/contracts"
	"example.com/paleo-prep-station/internal/httpapi"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/server"
	"example.com/paleo-prep-station/internal/station"
)

// 退出码：0-4 与 server 包一致（含 listen/serve/shutdown 阶段码），
// 5 起为检查自有阶段，任何失败都能据此定位到具体阶段。
const (
	exitOK       = server.ExitOK
	exitConfig   = server.ExitConfig
	exitReady    = 5
	exitProbe    = 6
	exitWorkflow = 7
)

// checkError 把一次检查失败绑定到具体阶段。
type checkError struct {
	stage string
	err   error
}

func (e *checkError) Error() string { return fmt.Sprintf("stage %s: %v", e.stage, e.err) }
func (e *checkError) Unwrap() error { return e.err }

type specimenView struct {
	ID            string `json:"id"`
	CatalogNumber string `json:"catalog_number"`
	Status        string `json:"status"`
}

type planView struct {
	ID         string `json:"id"`
	SpecimenID string `json:"specimen_id"`
	Status     string `json:"status"`
}

type planSnapshot struct {
	Plan     planView `json:"plan"`
	Progress []struct {
		Percent int `json:"percent"`
	} `json:"progress"`
}

type releaseView struct {
	ID         string `json:"id"`
	SpecimenID string `json:"specimen_id"`
}

// workflow 返回确定性的状态摘要，便于跨配置对比业务结果。
type workflow func(client *http.Client, baseURL string) (summary string, err error)

var workflows = map[string]workflow{
	"register-specimen": checkRegisterSpecimen,
	"approve-prep-plan": checkApprovePrepPlan,
	"log-preparation":   checkLogPreparation,
	"release-for-study": checkReleaseForStudy,
}

// workflowOrder 固定 --workflow all 的执行顺序，保证输出可重复对比。
var workflowOrder = []string{
	"register-specimen",
	"approve-prep-plan",
	"log-preparation",
	"release-for-study",
}

func main() {
	os.Exit(run())
}

func run() int {
	cfg := config.Default()
	// 检查进程默认使用临时端口，避免与运行中的服务或并行的其他检查冲突。
	cfg.Server.Addr = "127.0.0.1:0"

	fs := flag.NewFlagSet("workflowcheck", flag.ContinueOnError)
	workflowFlag := fs.String("workflow", "all", "workflow id to check, or all")
	if err := cfg.RegisterFlags(fs, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return exitConfig
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return exitConfig
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return exitConfig
	}

	selected, err := selectWorkflows(*workflowFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return exitConfig
	}

	log.Printf("effective config: %s", cfg.String())
	for _, id := range selected {
		if err := runWorkflow(cfg, id); err != nil {
			fmt.Fprintln(os.Stderr, "workflow check failed:", err)
			return exitCodeFor(err)
		}
	}
	fmt.Printf("workflow check passed: %s\n", *workflowFlag)
	return exitOK
}

func selectWorkflows(requested string) ([]string, error) {
	if requested == "all" {
		return workflowOrder, nil
	}
	if _, ok := workflows[requested]; ok {
		return []string{requested}, nil
	}
	return nil, fmt.Errorf("unknown workflow %q", requested)
}

func exitCodeFor(err error) int {
	var stageErr *server.StageError
	if errors.As(err, &stageErr) {
		return server.ExitCode(stageErr)
	}
	var checkErr *checkError
	if errors.As(err, &checkErr) {
		switch checkErr.stage {
		case "ready":
			return exitReady
		case "probe":
			return exitProbe
		case "workflow":
			return exitWorkflow
		}
	}
	return exitConfig
}

// runWorkflow 在独立的服务生命周期内执行一条业务链路：
// 监听 -> 就绪 -> 并发探针 -> 工作流请求 -> 优雅关闭。
// 无论成功或失败，返回前服务都已关闭，监听与 goroutine 归零。
func runWorkflow(cfg config.Config, id string) error {
	handler := httpapi.NewRouter(station.New(repository.NewMemory()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan net.Addr, 1)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Run(ctx, cfg.Server, handler, nil, ready)
	}()

	var baseURL string
	select {
	case addr := <-ready:
		baseURL = "http://" + addr.String()
	case err := <-serveErr:
		return err
	case <-time.After(cfg.Probe.ReadyTimeout):
		return stopAndJoin(cancel, serveErr, nil, &checkError{
			stage: "ready",
			err:   fmt.Errorf("listener not ready within %s", cfg.Probe.ReadyTimeout),
		})
	}

	client, closeIdle := cfg.Probe.Client()
	if err := waitReady(client, baseURL, cfg.Probe); err != nil {
		return stopAndJoin(cancel, serveErr, closeIdle, &checkError{stage: "ready", err: err})
	}
	if err := probeBurst(client, baseURL, cfg.Probe); err != nil {
		return stopAndJoin(cancel, serveErr, closeIdle, &checkError{stage: "probe", err: err})
	}

	summary, workflowErr := workflows[id](client, baseURL)
	if err := stopAndJoin(cancel, serveErr, closeIdle, nil); err != nil {
		return err
	}
	if workflowErr != nil {
		return &checkError{stage: "workflow", err: workflowErr}
	}
	fmt.Printf("workflow-status %s: %s\n", id, summary)
	return nil
}

// stopAndJoin 先排空客户端连接池，再触发优雅关闭并等待服务退出，
// 保证调用方观察到零残留；排空可以让服务端对竞态预拨的 StateNew
// 连接立即收到 EOF，避免短关闭期限下与读头超时撞车。关闭本身失败时
// 优先返回阶段错误，否则透传待定错误。
func stopAndJoin(cancel context.CancelFunc, serveErr <-chan error, closeIdle func(), pending error) error {
	if closeIdle != nil {
		closeIdle()
	}
	cancel()
	if err := <-serveErr; err != nil {
		return err
	}
	return pending
}

// waitReady 在探针就绪预算内轮询探针路径，直到返回 200。
func waitReady(client *http.Client, baseURL string, probe config.Probe) error {
	deadline := time.Now().Add(probe.ReadyTimeout)
	url := baseURL + probe.Path
	var last error
	for {
		status, err := get(client, url)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("probe %s returned %d", url, status)
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// probeBurst 以配置化并发度同时请求探针路径，验证服务在并发探针下
// 仍满足探针超时边界；任何一路失败都计入 probe 阶段。
func probeBurst(client *http.Client, baseURL string, probe config.Probe) error {
	url := baseURL + probe.Path
	var wg sync.WaitGroup
	errs := make(chan error, probe.Concurrency)
	for i := 0; i < probe.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := get(client, url)
			switch {
			case err != nil:
				errs <- err
			case status != http.StatusOK:
				errs <- fmt.Errorf("probe %s returned %d", url, status)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func get(client *http.Client, url string) (int, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return 0, err
	}
	return response.StatusCode, nil
}

func checkRegisterSpecimen(client *http.Client, baseURL string) (string, error) {
	specimen, err := registerSpecimen(client, baseURL)
	if err != nil {
		return "", err
	}
	if specimen.Status != "registered" {
		return "", fmt.Errorf("expected registered status, got %s", specimen.Status)
	}
	var fetched specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
		return "", err
	}
	if fetched.ID != specimen.ID {
		return "", errors.New("registered specimen cannot be read back")
	}
	return fmt.Sprintf("specimen=%s", fetched.Status), nil
}

func checkApprovePrepPlan(client *http.Client, baseURL string) (string, error) {
	specimen, err := registerSpecimen(client, baseURL)
	if err != nil {
		return "", err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return "", err
	}
	var approved planView
	if err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"},
		http.StatusOK,
		&approved,
	); err != nil {
		return "", err
	}
	if approved.Status != "approved" {
		return "", fmt.Errorf("expected approved plan, got %s", approved.Status)
	}
	var fetchedSpecimen specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
		return "", err
	}
	if fetchedSpecimen.Status != "preparing" {
		return "", fmt.Errorf("expected preparing specimen, got %s", fetchedSpecimen.Status)
	}
	return fmt.Sprintf("specimen=%s plan=%s", fetchedSpecimen.Status, approved.Status), nil
}

func checkLogPreparation(client *http.Client, baseURL string) (string, error) {
	specimen, plan, err := completedPlanSetup(client, baseURL)
	if err != nil {
		return "", err
	}
	if err := recordProgress(client, baseURL, plan.ID, 35, "矩阵剥离"); err != nil {
		return "", err
	}
	if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
		return "", err
	}
	var snapshot planSnapshot
	if err := requestJSON(client, http.MethodGet, baseURL+"/plans/"+plan.ID, nil, http.StatusOK, &snapshot); err != nil {
		return "", err
	}
	if snapshot.Plan.Status != "completed" || len(snapshot.Progress) != 2 {
		return "", errors.New("plan did not complete with exactly two progress entries")
	}
	var fetchedSpecimen specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
		return "", err
	}
	if fetchedSpecimen.Status != "stabilized" {
		return "", fmt.Errorf("expected stabilized specimen, got %s", fetchedSpecimen.Status)
	}
	return fmt.Sprintf("specimen=%s plan=%s progress=%d", fetchedSpecimen.Status, snapshot.Plan.Status, len(snapshot.Progress)), nil
}

func checkReleaseForStudy(client *http.Client, baseURL string) (string, error) {
	specimen, plan, err := completedPlanSetup(client, baseURL)
	if err != nil {
		return "", err
	}
	if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
		return "", err
	}
	var release releaseView
	if err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens/"+specimen.ID+"/release",
		contracts.ReleaseRequest{
			Purpose:    "比较解剖研究",
			Recipient:  "地层研究组",
			ApprovedBy: "keeper-zhou",
		},
		http.StatusCreated,
		&release,
	); err != nil {
		return "", err
	}
	if release.SpecimenID != specimen.ID {
		return "", errors.New("release record references the wrong specimen")
	}
	var fetched specimenView
	if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
		return "", err
	}
	if fetched.Status != "available" {
		return "", fmt.Errorf("expected available specimen, got %s", fetched.Status)
	}
	return fmt.Sprintf("specimen=%s release=recorded", fetched.Status), nil
}

func registerSpecimen(client *http.Client, baseURL string) (specimenView, error) {
	var specimen specimenView
	err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens",
		contracts.RegisterSpecimenRequest{
			CatalogNumber: "PPS-2026-001",
			FieldLabel:    "北坡-07",
			TaxonHint:     "大型脊椎动物",
			MatrixKind:    "粉砂岩",
			JacketNotes:   "夹克边角完好",
		},
		http.StatusCreated,
		&specimen,
	)
	return specimen, err
}

func createPlan(client *http.Client, baseURL, specimenID string) (planView, error) {
	var plan planView
	err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens/"+specimenID+"/plans",
		contracts.CreatePlanRequest{
			Objective:      "暴露主要关节面",
			Method:         "手工显微剥离",
			Adhesive:       "可逆树脂",
			TargetExposure: 80,
		},
		http.StatusCreated,
		&plan,
	)
	return plan, err
}

func completedPlanSetup(client *http.Client, baseURL string) (specimenView, planView, error) {
	specimen, err := registerSpecimen(client, baseURL)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	plan, err := createPlan(client, baseURL, specimen.ID)
	if err != nil {
		return specimenView{}, planView{}, err
	}
	var approved planView
	if err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/plans/"+plan.ID+"/approval",
		contracts.ApprovalRequest{ApprovedBy: "curator-lin"},
		http.StatusOK,
		&approved,
	); err != nil {
		return specimenView{}, planView{}, err
	}
	return specimen, approved, nil
}

func recordProgress(client *http.Client, baseURL, planID string, percent int, stage string) error {
	var ignored struct{}
	return requestJSON(
		client,
		http.MethodPost,
		baseURL+"/plans/"+planID+"/progress",
		contracts.ProgressRequest{
			Stage:      stage,
			Percent:    percent,
			Note:       "本地链路检查",
			RecordedBy: "preparator-chen",
		},
		http.StatusCreated,
		&ignored,
	)
}

func requestJSON(
	client *http.Client,
	method string,
	url string,
	body any,
	wantStatus int,
	target any,
) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, payload)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("request %s %s returned %d: %s", method, url, response.StatusCode, string(raw))
	}
	if target == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode response for %s %s: %w", method, url, err)
	}
	return nil
}
