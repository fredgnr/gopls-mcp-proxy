package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fredgnr/gopls-mcp-proxy/internal/backend"
	"github.com/fredgnr/gopls-mcp-proxy/internal/buildinfo"
	"github.com/fredgnr/gopls-mcp-proxy/internal/launcher"
	"github.com/fredgnr/gopls-mcp-proxy/internal/proxy"
	"github.com/fredgnr/gopls-mcp-proxy/internal/workspace"
)

type paths []string

func (p *paths) String() string { return strings.Join(*p, ",") }
func (p *paths) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*p = append(*p, value)
	return nil
}

type removedFlag string

func (f removedFlag) String() string   { return "" }
func (f removedFlag) IsBoolFlag() bool { return true }
func (f removedFlag) Set(string) error {
	return fmt.Errorf("--%s was removed: set the caller's working directory, use --watch-root for extra inputs, and start with -- program argv (for example -- gopls mcp)", f)
}

type launchMode string

const (
	defaultGopls    launchMode = "default-gopls"
	explicitBackend launchMode = "explicit"
)

type options struct {
	cfg                          proxy.Config
	mode                         launchMode
	program                      string
	argv                         []string
	watchRoots, include, exclude paths
	goDiscovery                  bool
	shutdown                     time.Duration
	version, help                bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("proxy stopped", "error", err)
		os.Exit(1)
	}
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	o := options{cfg: proxy.DefaultConfig()}
	boundary := slices.Index(args, "--")
	flagsArgs := args
	if boundary >= 0 {
		flagsArgs = args[:boundary]
	}
	flags := flag.NewFlagSet("gopls-mcp-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	for _, name := range []string{"gopls", "workspace", "replace-dir"} {
		flags.Var(removedFlag(name), name, "removed; use caller cwd, --watch-root and -- program argv")
	}
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gopls-mcp-proxy [proxy options]")
		fmt.Fprintln(stderr, "       gopls-mcp-proxy [proxy options] -- program [backend arguments...]")
		fmt.Fprintln(stderr, "Without --, start gopls mcp using the physical proxy directory, then absolute PATH directories.")
		fmt.Fprintln(stderr, "The backend inherits cwd and environment. Arguments after -- are passed unchanged.")
		fmt.Fprintln(stderr, "Keep the original gopls binary. When migrating args=[\"mcp\"], remove args or use -- gopls mcp.")
		flags.PrintDefaults()
	}
	flags.Var(&o.watchRoots, "watch-root", "additional input directory or file (repeatable; relative to cwd)")
	flags.Var(&o.include, "include", "include root-relative glob (repeatable; supports **)")
	flags.Var(&o.exclude, "exclude", "exclude root-relative glob (repeatable; exclusion wins)")
	flags.StringVar(&o.cfg.RefreshMode, "refresh-mode", o.cfg.RefreshMode, "backend refresh policy: on-change or per-call (fresh backend for each tools/call)")
	flags.StringVar(&o.cfg.BackendLifecycle, "backend-lifecycle", o.cfg.BackendLifecycle, "backend residency: resident or on-demand (requires per-call)")
	flags.DurationVar(&o.cfg.BackendIdleTimeout, "backend-idle-timeout", o.cfg.BackendIdleTimeout, "on-demand idle grace after initialization or ordinary requests; completed tools release immediately when safe")
	flags.BoolVar(&o.goDiscovery, "go-discovery", false, "optionally discover additional Go workspace input roots")
	flags.DurationVar(&o.cfg.Debounce, "debounce", o.cfg.Debounce, "quiet window before refreshing changed inputs")
	flags.DurationVar(&o.cfg.MaxDebounce, "max-debounce", o.cfg.MaxDebounce, "maximum event merging window")
	flags.DurationVar(&o.cfg.PollInterval, "poll-interval", o.cfg.PollInterval, "full content fingerprint polling interval")
	flags.DurationVar(&o.cfg.DrainTimeout, "drain-timeout", o.cfg.DrainTimeout, "maximum wait for active finite requests")
	flags.DurationVar(&o.cfg.StartupTimeout, "startup-timeout", o.cfg.StartupTimeout, "backend startup and protocol recovery deadline")
	flags.DurationVar(&o.cfg.RefreshTimeout, "refresh-timeout", o.cfg.RefreshTimeout, "total recovery budget per invalidation or on-demand wake")
	flags.DurationVar(&o.cfg.CleanupTimeout, "cleanup-timeout", o.cfg.CleanupTimeout, "final process cleanup budget")
	flags.DurationVar(&o.cfg.RequestTimeout, "request-timeout", o.cfg.RequestTimeout, "total finite request admission and execution budget")
	flags.DurationVar(&o.cfg.RetryMin, "retry-min", o.cfg.RetryMin, "initial restart backoff")
	flags.DurationVar(&o.cfg.RetryMax, "retry-max", o.cfg.RetryMax, "maximum restart backoff")
	flags.IntVar(&o.cfg.QueueLimit, "queue-limit", o.cfg.QueueLimit, "maximum queued requests; batches count each request")
	flags.IntVar(&o.cfg.QueueBytes, "queue-bytes", o.cfg.QueueBytes, "maximum queued message bytes")
	flags.IntVar(&o.cfg.MaxFrameBytes, "max-frame-bytes", o.cfg.MaxFrameBytes, "maximum bytes in one stdio frame")
	flags.IntVar(&o.cfg.MaxConcurrent, "max-concurrent", o.cfg.MaxConcurrent, "maximum executing finite requests")
	flags.BoolVar(&o.cfg.DisableWatcher, "disable-watcher", false, "use content polling and request checks without filesystem events")
	flags.DurationVar(&o.shutdown, "shutdown-timeout", 2*time.Second, "grace period after stdin EOF and after TERM")
	flags.BoolVar(&o.version, "version", false, "print proxy version to stderr and exit")
	if err := flags.Parse(flagsArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			o.help = true
			return o, nil
		}
		return o, err
	}
	if flags.NArg() != 0 {
		return o, errors.New("unexpected arguments before --; use -- program argv, or remove args=[\"mcp\"] to use the default gopls launcher")
	}
	if o.version || o.help {
		return o, nil
	}
	if boundary < 0 {
		o.mode = defaultGopls
		o.argv = []string{"mcp"}
	} else {
		if boundary+1 >= len(args) || args[boundary+1] == "" {
			return o, errors.New("backend program is required after --: gopls-mcp-proxy [proxy options] -- program [arguments...]")
		}
		o.mode = explicitBackend
		o.program = args[boundary+1]
		o.argv = slices.Clone(args[boundary+2:])
	}
	if err := o.cfg.Validate(); err != nil {
		return o, err
	}
	if o.shutdown <= 0 {
		return o, errors.New("shutdown-timeout must be positive")
	}
	return o, nil
}

func run(ctx context.Context, args []string) error {
	return runWithSetup(ctx, args, os.Stderr, runConfigured)
}

// Keep local diagnostics ahead of every runtime dependency, including cwd,
// executable identity, backend discovery, and workspace scanning.
func runWithSetup(ctx context.Context, args []string, stderr io.Writer, setup func(context.Context, options) error) error {
	o, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	if o.help {
		return nil
	}
	if o.version {
		fmt.Fprintf(stderr, "gopls-mcp-proxy %s (commit %s)\n", buildinfo.Version, buildinfo.Commit)
		return nil
	}
	return setup(ctx, o)
}

func runConfigured(ctx context.Context, o options) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	o.cfg.Logger = logger
	env := os.Environ()
	self, err := launcher.CaptureSelf()
	if err != nil {
		return err
	}
	selectionSource := "explicit"
	if o.mode == defaultGopls {
		spec, err := launcher.ResolveDefault(self, environmentValue(env, "PATH"))
		if err != nil {
			return err
		}
		o.program, o.argv, selectionSource = spec.Locator, spec.Args, spec.SelectionSource
	}
	factory, err := backend.NewFactory(backend.Config{Executable: o.program, Args: o.argv, Workspace: cwd, Env: env, ShutdownTimeout: o.shutdown, CleanupTimeout: o.cfg.CleanupTimeout, Logger: logger, BeforeStart: self.Check})
	if err != nil {
		return err
	}
	if err := self.Check(ctx, factory.Locator()); err != nil {
		return err
	}
	trackerCfg := workspace.Config{Workspace: cwd, WatchRoots: o.watchRoots, Include: o.include, Exclude: o.exclude, GoDiscovery: o.goDiscovery}
	if o.goDiscovery {
		trackerCfg.WorkFile = environmentValue(env, "GOWORK")
	}
	tracker, err := workspace.New(trackerCfg, logger)
	if err != nil {
		return err
	}
	logger.Info("proxy starting", "version", buildinfo.Version, "commit", buildinfo.Commit, "mode", o.mode, "refresh_mode", o.cfg.RefreshMode, "backend_lifecycle", o.cfg.BackendLifecycle, "backend_idle_timeout", o.cfg.BackendIdleTimeout, "selection_source", selectionSource, "backend", factory.Locator(), "cwd", cwd)
	return proxy.Run(ctx, o.cfg, tracker, factory, os.Stdin, os.Stdout)
}

func environmentValue(env []string, name string) string {
	var value string
	for _, entry := range env {
		if found, ok := strings.CutPrefix(entry, name+"="); ok {
			value = found
		}
	}
	return value
}
