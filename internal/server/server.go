// Package server 以显式阶段运行 HTTP 服务：监听、服务、关闭。
//
// 每个阶段的失败都绑定阶段标签并映射到固定退出码，外部验证流程
// 可以据此把任何失败定位到具体阶段。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"example.com/paleo-prep-station/internal/config"
)

// Stage 标记进程运行所处的阶段。
type Stage string

const (
	StageListen   Stage = "listen"
	StageServe    Stage = "serve"
	StageShutdown Stage = "shutdown"
)

// StageError 把一次失败绑定到具体阶段。
type StageError struct {
	Stage Stage
	Err   error
}

func (e *StageError) Error() string { return fmt.Sprintf("stage %s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

// 进程退出码：按失败阶段区分，便于外部验证脚本断言。
const (
	ExitOK       = 0
	ExitConfig   = 1
	ExitListen   = 2
	ExitServe    = 3
	ExitShutdown = 4
)

// ExitCode 把运行错误映射为进程退出码。
func ExitCode(err error) int {
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		return ExitConfig
	}
	switch stageErr.Stage {
	case StageListen:
		return ExitListen
	case StageServe:
		return ExitServe
	case StageShutdown:
		return ExitShutdown
	default:
		return ExitConfig
	}
}

// Run 启动 HTTP 服务并阻塞到 ctx 取消或服务失败。
//
// 监听失败（例如端口冲突）在 listen 阶段返回；ctx 取消后在
// cfg.ShutdownTimeout 期限内优雅关闭，超期或失败计入 shutdown 阶段。
// ready 非 nil 时，监听成功后通过它通告实际地址（非阻塞发送），
// 调用方可以借此使用临时端口（例如 127.0.0.1:0）。
func Run(ctx context.Context, cfg config.Server, handler http.Handler, logf func(string, ...any), ready chan<- net.Addr) error {
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return &StageError{Stage: StageListen, Err: err}
	}
	httpServer := cfg.HTTPServer(handler)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	if logf != nil {
		logf("listening on %s", listener.Addr())
	}
	if ready != nil {
		select {
		case ready <- listener.Addr():
		default:
		}
	}

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return &StageError{Stage: StageServe, Err: err}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return &StageError{Stage: StageShutdown, Err: err}
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return &StageError{Stage: StageServe, Err: err}
		}
		return nil
	}
}
