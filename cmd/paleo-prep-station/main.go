// Command paleo-prep-station 是古生物修复制备台的 HTTP 进程入口。
//
// 监听地址、读写与空闲超时、关闭期限全部来自 internal/config 的显式配置
// （flag 优先，PPS_* 环境变量次之，内置默认值兜底），进程生命周期按
// listen/serve/shutdown 阶段上报失败，并映射到 server 包的固定退出码。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"example.com/paleo-prep-station/internal/config"
	"example.com/paleo-prep-station/internal/httpapi"
	"example.com/paleo-prep-station/internal/repository"
	"example.com/paleo-prep-station/internal/server"
	"example.com/paleo-prep-station/internal/station"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg := config.Default()
	fs := flag.NewFlagSet("paleo-prep-station", flag.ContinueOnError)
	if err := cfg.RegisterFlags(fs, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return server.ExitConfig
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return server.ExitOK
		}
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return server.ExitConfig
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "stage config:", err)
		return server.ExitConfig
	}

	log.Printf("effective config: %s", cfg.String())

	handler := httpapi.NewRouter(station.New(repository.NewMemory()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, cfg.Server, handler, log.Printf, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return server.ExitCode(err)
	}
	return server.ExitOK
}
