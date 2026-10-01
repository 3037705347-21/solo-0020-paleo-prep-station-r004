// Package config 集中定义进程运行与验证探针的配置入口。
//
// 监听地址、读写与空闲超时、关闭期限和探针资源都从这里显式进入：
// flag 取值优先，环境变量（PPS_*）次之，内置默认值兜底。
// 默认值与此前写死在命令入口的参数一致。
package config

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server 描述 HTTP 监听与连接生命周期的资源边界。
type Server struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// Probe 描述对运行中服务发起健康探针的资源边界。
type Probe struct {
	Path         string
	Timeout      time.Duration
	Concurrency  int
	ReadyTimeout time.Duration
}

// Config 聚合进程运行与验证所需的全部配置。
type Config struct {
	Server Server
	Probe  Probe
}

// Default 返回基线默认配置。
func Default() Config {
	return Config{
		Server: Server{
			Addr:              "127.0.0.1:18133",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
			ShutdownTimeout:   5 * time.Second,
		},
		Probe: Probe{
			Path:         "/healthz",
			Timeout:      5 * time.Second,
			Concurrency:  8,
			ReadyTimeout: 5 * time.Second,
		},
	}
}

// String 输出一份单行有效配置摘要，便于在验证日志中核对取值来源。
func (c Config) String() string {
	return fmt.Sprintf(
		"addr=%s read-header-timeout=%s read-timeout=%s write-timeout=%s idle-timeout=%s shutdown-timeout=%s probe-path=%s probe-timeout=%s probe-concurrency=%d probe-ready-timeout=%s",
		c.Server.Addr,
		c.Server.ReadHeaderTimeout,
		c.Server.ReadTimeout,
		c.Server.WriteTimeout,
		c.Server.IdleTimeout,
		c.Server.ShutdownTimeout,
		c.Probe.Path,
		c.Probe.Timeout,
		c.Probe.Concurrency,
		c.Probe.ReadyTimeout,
	)
}

// RegisterFlags 把全部配置项绑定到 fs。
//
// 环境变量提供 flag 的默认值，显式 flag 始终优先；环境变量取值非法时
// 立即返回错误，不会静默回退。
func (c *Config) RegisterFlags(fs *flag.FlagSet, getenv func(string) string) error {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	stringEnv := func(key, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	durationEnv := func(key string, fallback time.Duration) (time.Duration, error) {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return fallback, nil
		}
		value, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("环境变量 %s=%q 不是合法时长: %w", key, raw, err)
		}
		return value, nil
	}
	intEnv := func(key string, fallback int) (int, error) {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return fallback, nil
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("环境变量 %s=%q 不是合法整数: %w", key, raw, err)
		}
		return value, nil
	}

	server := &c.Server
	probe := &c.Probe
	var err error

	fs.StringVar(&server.Addr, "addr", stringEnv("PPS_ADDR", server.Addr),
		"HTTP listen address (env PPS_ADDR)")
	if server.ReadHeaderTimeout, err = durationEnv("PPS_READ_HEADER_TIMEOUT", server.ReadHeaderTimeout); err != nil {
		return err
	}
	fs.DurationVar(&server.ReadHeaderTimeout, "read-header-timeout", server.ReadHeaderTimeout,
		"HTTP read header timeout (env PPS_READ_HEADER_TIMEOUT)")
	if server.ReadTimeout, err = durationEnv("PPS_READ_TIMEOUT", server.ReadTimeout); err != nil {
		return err
	}
	fs.DurationVar(&server.ReadTimeout, "read-timeout", server.ReadTimeout,
		"HTTP read timeout (env PPS_READ_TIMEOUT)")
	if server.WriteTimeout, err = durationEnv("PPS_WRITE_TIMEOUT", server.WriteTimeout); err != nil {
		return err
	}
	fs.DurationVar(&server.WriteTimeout, "write-timeout", server.WriteTimeout,
		"HTTP write timeout (env PPS_WRITE_TIMEOUT)")
	if server.IdleTimeout, err = durationEnv("PPS_IDLE_TIMEOUT", server.IdleTimeout); err != nil {
		return err
	}
	fs.DurationVar(&server.IdleTimeout, "idle-timeout", server.IdleTimeout,
		"HTTP idle keep-alive timeout (env PPS_IDLE_TIMEOUT)")
	if server.ShutdownTimeout, err = durationEnv("PPS_SHUTDOWN_TIMEOUT", server.ShutdownTimeout); err != nil {
		return err
	}
	fs.DurationVar(&server.ShutdownTimeout, "shutdown-timeout", server.ShutdownTimeout,
		"graceful shutdown deadline (env PPS_SHUTDOWN_TIMEOUT)")

	fs.StringVar(&probe.Path, "probe-path", stringEnv("PPS_PROBE_PATH", probe.Path),
		"health probe path (env PPS_PROBE_PATH)")
	if probe.Timeout, err = durationEnv("PPS_PROBE_TIMEOUT", probe.Timeout); err != nil {
		return err
	}
	fs.DurationVar(&probe.Timeout, "probe-timeout", probe.Timeout,
		"per-request probe timeout (env PPS_PROBE_TIMEOUT)")
	if probe.Concurrency, err = intEnv("PPS_PROBE_CONCURRENCY", probe.Concurrency); err != nil {
		return err
	}
	fs.IntVar(&probe.Concurrency, "probe-concurrency", probe.Concurrency,
		"concurrent probe workers (env PPS_PROBE_CONCURRENCY)")
	if probe.ReadyTimeout, err = durationEnv("PPS_PROBE_READY_TIMEOUT", probe.ReadyTimeout); err != nil {
		return err
	}
	fs.DurationVar(&probe.ReadyTimeout, "probe-ready-timeout", probe.ReadyTimeout,
		"startup readiness wait budget (env PPS_PROBE_READY_TIMEOUT)")
	return nil
}

// Validate 校验最终配置，拒绝任何放宽资源边界的取值（例如零超时）。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return fmt.Errorf("监听地址不能为空")
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"read-header-timeout", c.Server.ReadHeaderTimeout},
		{"read-timeout", c.Server.ReadTimeout},
		{"write-timeout", c.Server.WriteTimeout},
		{"idle-timeout", c.Server.IdleTimeout},
		{"shutdown-timeout", c.Server.ShutdownTimeout},
		{"probe-timeout", c.Probe.Timeout},
		{"probe-ready-timeout", c.Probe.ReadyTimeout},
	}
	for _, item := range durations {
		if item.value <= 0 {
			return fmt.Errorf("%s 必须为正数，当前为 %s", item.name, item.value)
		}
	}
	if c.Probe.Concurrency < 1 {
		return fmt.Errorf("probe-concurrency 至少为 1，当前为 %d", c.Probe.Concurrency)
	}
	if !strings.HasPrefix(c.Probe.Path, "/") {
		return fmt.Errorf("probe-path 必须以 / 开头，当前为 %q", c.Probe.Path)
	}
	return nil
}

// HTTPServer 按配置构造 *http.Server，把超时边界落到传输层。
func (s Server) HTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              s.Addr,
		Handler:           handler,
		ReadHeaderTimeout: s.ReadHeaderTimeout,
		ReadTimeout:       s.ReadTimeout,
		WriteTimeout:      s.WriteTimeout,
		IdleTimeout:       s.IdleTimeout,
	}
}

// Client 按探针配置构造带超时边界的 *http.Client。
//
// 返回的客户端使用独立 Transport（不共享 http.DefaultTransport），
// closeIdle 用于在发起优雅关闭前排空客户端连接池：包括已回到池中的
// keep-alive 连接，以及竞态预拨但尚未发送请求的连接（服务端仍处于
// StateNew）。不排空时，标准库 Shutdown 会把 StateNew 连接按 5 秒
// 旧连接处理（见 net/http Issue 22682），短关闭期限下可能与
// read-header-timeout 同时到期而误报 shutdown 阶段失败。
func (p Probe) Client() (client *http.Client, closeIdle func()) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   p.Timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
	}
	return &http.Client{Timeout: p.Timeout, Transport: transport}, transport.CloseIdleConnections
}
