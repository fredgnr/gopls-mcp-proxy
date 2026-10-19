package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/proxy"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type paths []string

func (p *paths) String() string         { return strings.Join(*p, ",") }
func (p *paths) Set(value string) error { *p = append(*p, value); return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cfg := proxy.DefaultConfig()
	flags := flag.NewFlagSet("gopls-mcp-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	gopls := flags.String("gopls", "gopls", "gopls executable (fixed for this proxy session)")
	root := flags.String("workspace", ".", "workspace directory")
	var replacements paths
	flags.Var(&replacements, "replace-dir", "additional local replace directory to track (repeatable, relative to workspace)")
	flags.DurationVar(&cfg.Debounce, "debounce", cfg.Debounce, "quiet window before verifying filesystem events")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "full fingerprint polling interval")
	flags.DurationVar(&cfg.DrainTimeout, "drain-timeout", cfg.DrainTimeout, "maximum wait for active requests before stopping gopls")
	flags.DurationVar(&cfg.StartupTimeout, "startup-timeout", cfg.StartupTimeout, "backend initialization deadline")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "total queue, check and execution budget per tool call")
	flags.DurationVar(&cfg.RetryMin, "retry-min", cfg.RetryMin, "initial restart backoff")
	flags.DurationVar(&cfg.RetryMax, "retry-max", cfg.RetryMax, "maximum restart backoff")
	flags.IntVar(&cfg.QueueLimit, "queue-limit", cfg.QueueLimit, "maximum pending tool calls")
	flags.IntVar(&cfg.MaxConcurrent, "max-concurrent", cfg.MaxConcurrent, "maximum executing tool calls")
	shutdown := flags.Duration("shutdown-timeout", 2*time.Second, "grace period for each shutdown stage")
	version := flags.Bool("version", false, "print version to stderr and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *version {
		fmt.Fprintln(os.Stderr, "gopls-mcp-proxy 0.1.0")
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if *shutdown <= 0 {
		return fmt.Errorf("shutdown-timeout must be positive")
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	env, err := goEnvironment(ctx, absolute)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg.Logger = logger
	tracker, err := workspace.New(workspace.Config{Workspace: absolute, WorkFile: env["GOWORK"], ReplaceDirs: replacements, ExcludeDirs: []string{env["GOMODCACHE"], env["GOROOT"]}}, logger)
	if err != nil {
		return err
	}
	factory, err := backend.NewFactory(backend.Config{Executable: *gopls, Workspace: absolute, ShutdownTimeout: *shutdown, Logger: logger})
	if err != nil {
		return err
	}
	supervisor, err := proxy.New(ctx, cfg, tracker, factory)
	if err != nil {
		return err
	}
	defer supervisor.Close()
	startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	tools, instructions, err := supervisor.WaitReady(startupCtx)
	cancel()
	if err != nil {
		return err
	}
	server := supervisor.Server(tools, instructions)
	err = server.Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Resolve cache exclusions and persisted GOWORK without loading the module graph.
// An unset GOWORK stays automatic so newly added/deleted ancestor go.work files
// are discovered on every scan. Explicit environment values override GOENV.
func goEnvironment(ctx context.Context, dir string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOENV", "GOMODCACHE", "GOROOT")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go env: %w: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("go env: %w", err)
	}
	values := map[string]string{}
	if err = json.Unmarshal(output, &values); err != nil {
		return nil, err
	}
	if file := values["GOENV"]; file != "" && file != "off" {
		data, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		scan := bufio.NewScanner(strings.NewReader(string(data)))
		for scan.Scan() {
			if v, ok := strings.CutPrefix(scan.Text(), "GOWORK="); ok {
				values["GOWORK"] = v
			}
		}
		if err := scan.Err(); err != nil {
			return nil, err
		}
	}
	if value := os.Getenv("GOWORK"); value != "" {
		values["GOWORK"] = value
	}
	return values, nil
}
