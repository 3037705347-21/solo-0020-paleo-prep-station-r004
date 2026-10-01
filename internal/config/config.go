// Package config turns the previously hard-coded process and probe
// parameters into one explicit, validated configuration surface shared by
// the service entrypoint and the workflow checks.
//
// Precedence for every field is: command-line flag > environment variable >
// default. The defaults are byte-for-byte the values the entrypoint used to
// embed literally, so loading no configuration reproduces the old behaviour.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ServerConfig configures the long-lived HTTP process.
type ServerConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// ProbeConfig configures the resources owned by a workflow check run.
type ProbeConfig struct {
	// BaseURL points at a running server. A check never starts its own
	// listener, so this is required.
	BaseURL string
	// RequestTimeout bounds each individual probe HTTP request.
	RequestTimeout time.Duration
	// CatalogNumber is the catalog number the check registers. Varying it
	// lets several probes run concurrently against one shared server without
	// tripping the (business-required) unique catalog number conflict.
	CatalogNumber string
}

// Defaults returns the values the commands shipped with before
// configuration was externalised.
func Defaults() ServerConfig {
	return ServerConfig{
		Addr:              "127.0.0.1:18133",
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   5 * time.Second,
	}
}

const (
	envAddr              = "PPS_ADDR"
	envReadHeaderTimeout = "PPS_READ_HEADER_TIMEOUT"
	envReadTimeout       = "PPS_READ_TIMEOUT"
	envWriteTimeout      = "PPS_WRITE_TIMEOUT"
	envIdleTimeout       = "PPS_IDLE_TIMEOUT"
	envShutdownTimeout   = "PPS_SHUTDOWN_TIMEOUT"

	envProbeBaseURL        = "PPS_PROBE_BASE_URL"
	envProbeRequestTimeout = "PPS_PROBE_REQUEST_TIMEOUT"
	envProbeCatalogNumber  = "PPS_PROBE_CATALOG_NUMBER"

	defaultProbeRequestTimeout = 5 * time.Second
	defaultProbeCatalogNumber  = "PPS-2026-001"
)

// envReader resolves environment variables into flag defaults and records
// malformed values so they surface together with validation errors.
type envReader struct{ problems []string }

func (e *envReader) str(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func (e *envReader) duration(key string, fallback time.Duration) time.Duration {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		e.problems = append(e.problems,
			fmt.Sprintf("%s=%q is not a valid Go duration (e.g. 5s, 1m)", key, value))
		return fallback
	}
	return parsed
}

// LoadServer parses the server configuration from args (typically
// os.Args[1:]) and the process environment. The returned errors describe
// the offending field and are safe to print as usage failures.
func LoadServer(args []string) (ServerConfig, error) {
	env := &envReader{}
	cfg := Defaults()
	cfg.Addr = env.str(envAddr, cfg.Addr)
	cfg.ReadHeaderTimeout = env.duration(envReadHeaderTimeout, cfg.ReadHeaderTimeout)
	cfg.ReadTimeout = env.duration(envReadTimeout, cfg.ReadTimeout)
	cfg.WriteTimeout = env.duration(envWriteTimeout, cfg.WriteTimeout)
	cfg.IdleTimeout = env.duration(envIdleTimeout, cfg.IdleTimeout)
	cfg.ShutdownTimeout = env.duration(envShutdownTimeout, cfg.ShutdownTimeout)

	fs := flag.NewFlagSet("paleo-prep-station", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&cfg.Addr, "addr", cfg.Addr,
		"HTTP listen address (env "+envAddr+")")
	fs.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout,
		"maximum duration for reading the request headers (env "+envReadHeaderTimeout+")")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout,
		"maximum duration for reading the complete request (env "+envReadTimeout+")")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout,
		"maximum duration before timing out a response write (env "+envWriteTimeout+")")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout,
		"maximum idle time between keep-alive requests (env "+envIdleTimeout+")")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout,
		"grace period for in-flight requests on SIGINT/SIGTERM (env "+envShutdownTimeout+")")

	if err := fs.Parse(args); err != nil {
		return ServerConfig{}, err
	}
	if fs.NArg() > 0 {
		env.problems = append(env.problems,
			"unexpected positional arguments: "+strings.Join(fs.Args(), " "))
	}
	if err := cfg.Validate(); err != nil {
		env.problems = append(env.problems, err.Error())
	}
	if len(env.problems) > 0 {
		return ServerConfig{}, errors.New(strings.Join(env.problems, "; "))
	}
	return cfg, nil
}

// Validate enforces the resource boundary. Every timeout must be positive
// and the read header timeout must not exceed the whole read timeout; the
// point of externalising the values is proving the same boundary holds, not
// allowing it to be silently disabled with a zero duration.
func (c ServerConfig) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Addr) == "" {
		problems = append(problems, "addr must not be empty")
	} else if !hasHostPort(c.Addr) {
		problems = append(problems, fmt.Sprintf("addr %q must be host:port", c.Addr))
	}
	if c.ReadHeaderTimeout <= 0 {
		problems = append(problems, "read-header-timeout must be greater than zero")
	}
	if c.ReadTimeout <= 0 {
		problems = append(problems, "read-timeout must be greater than zero")
	}
	if c.WriteTimeout <= 0 {
		problems = append(problems, "write-timeout must be greater than zero")
	}
	if c.IdleTimeout <= 0 {
		problems = append(problems, "idle-timeout must be greater than zero")
	}
	if c.ShutdownTimeout <= 0 {
		problems = append(problems, "shutdown-timeout must be greater than zero")
	}
	if c.ReadHeaderTimeout > 0 && c.ReadTimeout > 0 && c.ReadHeaderTimeout > c.ReadTimeout {
		problems = append(problems, "read-header-timeout must not exceed read-timeout")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Workflow identifiers accepted by the workflowcheck command. They mirror
// the four business workflows end to end and must not be renamed.
const (
	WorkflowRegisterSpecimen = "register-specimen"
	WorkflowApprovePrepPlan  = "approve-prep-plan"
	WorkflowLogPreparation   = "log-preparation"
	WorkflowReleaseForStudy  = "release-for-study"
)

// CheckConfig binds a single workflow run to its probe resources.
type CheckConfig struct {
	Workflow string
	Probe    ProbeConfig
}

// LoadCheck parses the workflowcheck configuration: which of the four
// business workflows to run and which probe resources it may use.
func LoadCheck(args []string) (CheckConfig, error) {
	env := &envReader{}
	cfg := CheckConfig{
		Probe: ProbeConfig{
			BaseURL:        env.str(envProbeBaseURL, ""),
			RequestTimeout: env.duration(envProbeRequestTimeout, defaultProbeRequestTimeout),
			CatalogNumber:  env.str(envProbeCatalogNumber, defaultProbeCatalogNumber),
		},
	}

	fs := flag.NewFlagSet("workflowcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&cfg.Workflow, "workflow", "",
		"workflow to check: register-specimen|approve-prep-plan|log-preparation|release-for-study")
	fs.StringVar(&cfg.Probe.BaseURL, "base-url", cfg.Probe.BaseURL,
		"base URL of a running paleo prep station server (env "+envProbeBaseURL+")")
	fs.DurationVar(&cfg.Probe.RequestTimeout, "request-timeout", cfg.Probe.RequestTimeout,
		"per-request timeout for probe calls (env "+envProbeRequestTimeout+")")
	fs.StringVar(&cfg.Probe.CatalogNumber, "catalog-number", cfg.Probe.CatalogNumber,
		"catalog number registered by the probe; vary it for concurrent runs (env "+envProbeCatalogNumber+")")

	if err := fs.Parse(args); err != nil {
		return CheckConfig{}, err
	}
	if fs.NArg() > 0 {
		env.problems = append(env.problems,
			"unexpected positional arguments: "+strings.Join(fs.Args(), " "))
	}
	switch cfg.Workflow {
	case WorkflowRegisterSpecimen, WorkflowApprovePrepPlan,
		WorkflowLogPreparation, WorkflowReleaseForStudy:
	default:
		env.problems = append(env.problems, fmt.Sprintf(
			"workflow must be one of %s, %s, %s, %s",
			WorkflowRegisterSpecimen, WorkflowApprovePrepPlan,
			WorkflowLogPreparation, WorkflowReleaseForStudy))
	}
	if err := cfg.Probe.Validate(); err != nil {
		env.problems = append(env.problems, err.Error())
	}
	if len(env.problems) > 0 {
		return CheckConfig{}, errors.New(strings.Join(env.problems, "; "))
	}
	cfg.Probe.BaseURL = strings.TrimRight(cfg.Probe.BaseURL, "/")
	return cfg, nil
}

// Validate makes the base URL explicit and mandatory: a probe must target a
// server whose lifecycle is managed out of band, it must never conjure its
// own listener.
func (c ProbeConfig) Validate() error {
	var problems []string
	if strings.TrimSpace(c.BaseURL) == "" {
		problems = append(problems, "base-url must point at a running server (set --base-url or "+envProbeBaseURL+")")
	} else {
		parsed, err := url.Parse(c.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			problems = append(problems, fmt.Sprintf("base-url %q must be an absolute http(s) URL", c.BaseURL))
		}
	}
	if c.RequestTimeout <= 0 {
		problems = append(problems, "request-timeout must be greater than zero")
	}
	if strings.TrimSpace(c.CatalogNumber) == "" {
		problems = append(problems, "catalog-number must not be empty")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Summary renders the effective server configuration for the startup log,
// proving which resource boundary the process actually bound to.
func (c ServerConfig) Summary() string {
	return fmt.Sprintf(
		"addr=%s read_header_timeout=%s read_timeout=%s write_timeout=%s idle_timeout=%s shutdown_timeout=%s",
		c.Addr, c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout, c.ShutdownTimeout,
	)
}

// Summary renders the effective probe configuration for run logs.
func (c ProbeConfig) Summary() string {
	return fmt.Sprintf("base_url=%s request_timeout=%s catalog_number=%s",
		c.BaseURL, c.RequestTimeout, c.CatalogNumber)
}

func hasHostPort(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return false
	}
	return true
}
