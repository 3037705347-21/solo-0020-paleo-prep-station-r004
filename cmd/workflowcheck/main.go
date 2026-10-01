package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"example.com/paleo-prep-station/internal/config"
	"example.com/paleo-prep-station/internal/contracts"
)

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

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.LoadCheck(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}

	client := &http.Client{Timeout: cfg.Probe.RequestTimeout}
	log.Printf("workflow check starting: workflow=%s %s", cfg.Workflow, cfg.Probe.Summary())

	if err := execute(cfg, client); err != nil {
		fmt.Fprintf(os.Stderr, "workflow check failed: workflow=%s %v\n", cfg.Workflow, err)
		return 1
	}
	fmt.Printf("workflow check passed: %s (catalog_number=%s)\n", cfg.Workflow, cfg.Probe.CatalogNumber)
	return 0
}

func execute(cfg config.CheckConfig, client *http.Client) error {
	baseURL := cfg.Probe.BaseURL
	catalogNumber := cfg.Probe.CatalogNumber

	switch cfg.Workflow {
	case config.WorkflowRegisterSpecimen:
		specimen, err := registerSpecimen(client, baseURL, catalogNumber)
		if err != nil {
			return fmt.Errorf("stage register-specimen/register: %w", err)
		}
		if specimen.Status != "registered" {
			return fmt.Errorf("stage register-specimen/register: expected registered status, got %s", specimen.Status)
		}
		var fetched specimenView
		if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
			return fmt.Errorf("stage register-specimen/read-back: %w", err)
		}
		if fetched.ID != specimen.ID {
			return fmt.Errorf("stage register-specimen/read-back: registered specimen cannot be read back")
		}
		return nil

	case config.WorkflowApprovePrepPlan:
		specimen, err := registerSpecimen(client, baseURL, catalogNumber)
		if err != nil {
			return fmt.Errorf("stage approve-prep-plan/register: %w", err)
		}
		plan, err := createPlan(client, baseURL, specimen.ID)
		if err != nil {
			return fmt.Errorf("stage approve-prep-plan/create-plan: %w", err)
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
			return fmt.Errorf("stage approve-prep-plan/approve: %w", err)
		}
		if approved.Status != "approved" {
			return fmt.Errorf("stage approve-prep-plan/approve: expected approved plan, got %s", approved.Status)
		}
		var fetchedSpecimen specimenView
		if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
			return fmt.Errorf("stage approve-prep-plan/read-specimen: %w", err)
		}
		if fetchedSpecimen.Status != "preparing" {
			return fmt.Errorf("stage approve-prep-plan/read-specimen: expected preparing specimen, got %s", fetchedSpecimen.Status)
		}
		return nil

	case config.WorkflowLogPreparation:
		specimen, plan, err := completedPlanSetup(client, baseURL, catalogNumber)
		if err != nil {
			return fmt.Errorf("stage log-preparation/setup: %w", err)
		}
		if err := recordProgress(client, baseURL, plan.ID, 35, "矩阵剥离"); err != nil {
			return fmt.Errorf("stage log-preparation/progress-partial: %w", err)
		}
		if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
			return fmt.Errorf("stage log-preparation/progress-final: %w", err)
		}
		var snapshot planSnapshot
		if err := requestJSON(client, http.MethodGet, baseURL+"/plans/"+plan.ID, nil, http.StatusOK, &snapshot); err != nil {
			return fmt.Errorf("stage log-preparation/read-plan: %w", err)
		}
		if snapshot.Plan.Status != "completed" || len(snapshot.Progress) != 2 {
			return fmt.Errorf("stage log-preparation/read-plan: plan did not complete with exactly two progress entries")
		}
		var fetchedSpecimen specimenView
		if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetchedSpecimen); err != nil {
			return fmt.Errorf("stage log-preparation/read-specimen: %w", err)
		}
		if fetchedSpecimen.Status != "stabilized" {
			return fmt.Errorf("stage log-preparation/read-specimen: expected stabilized specimen, got %s", fetchedSpecimen.Status)
		}
		return nil

	case config.WorkflowReleaseForStudy:
		specimen, plan, err := completedPlanSetup(client, baseURL, catalogNumber)
		if err != nil {
			return fmt.Errorf("stage release-for-study/setup: %w", err)
		}
		if err := recordProgress(client, baseURL, plan.ID, 100, "表面加固"); err != nil {
			return fmt.Errorf("stage release-for-study/complete: %w", err)
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
			return fmt.Errorf("stage release-for-study/release: %w", err)
		}
		if release.SpecimenID != specimen.ID {
			return fmt.Errorf("stage release-for-study/release: release record references the wrong specimen")
		}
		var fetched specimenView
		if err := requestJSON(client, http.MethodGet, baseURL+"/specimens/"+specimen.ID, nil, http.StatusOK, &fetched); err != nil {
			return fmt.Errorf("stage release-for-study/read-specimen: %w", err)
		}
		if fetched.Status != "available" {
			return fmt.Errorf("stage release-for-study/read-specimen: expected available specimen, got %s", fetched.Status)
		}
		return nil
	default:
		return fmt.Errorf("stage config: unknown workflow %q", cfg.Workflow)
	}
}

func registerSpecimen(client *http.Client, baseURL, catalogNumber string) (specimenView, error) {
	var specimen specimenView
	err := requestJSON(
		client,
		http.MethodPost,
		baseURL+"/specimens",
		contracts.RegisterSpecimenRequest{
			CatalogNumber: catalogNumber,
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

func completedPlanSetup(client *http.Client, baseURL, catalogNumber string) (specimenView, planView, error) {
	specimen, err := registerSpecimen(client, baseURL, catalogNumber)
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
