// Package backend owns a single gopls process and its independent MCP client session.
package backend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const ProtocolVersion = "2025-11-25"

type Session interface {
	Tools() []*mcp.Tool
	Instructions() string
	Call(context.Context, *mcp.CallToolParams, func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error)
	Done() <-chan struct{}
	Close()
}

type Factory func(context.Context, []string) (Session, error)

type Config struct {
	Executable string
	Workspace  string
	// Args is normally ["mcp"]. Tests use a standalone MCP fixture executable.
	Args            []string
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
	Stderr          io.Writer
}

// NewFactory resolves and fingerprints the executable once for the proxy lifetime.
func NewFactory(cfg Config) (Factory, error) {
	path, err := exec.LookPath(cfg.Executable)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	original, err := executableDigest(path)
	if err != nil {
		return nil, err
	}
	cfg.Executable = path
	if cfg.Args == nil {
		cfg.Args = []string{"mcp"}
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	return func(ctx context.Context, roots []string) (Session, error) {
		digest, err := executableDigest(path)
		if err != nil {
			return nil, err
		}
		if digest != original {
			return nil, fmt.Errorf("gopls executable changed; restart the proxy session")
		}
		return start(ctx, cfg, roots)
	}, nil
}

func executableDigest(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

type process struct {
	cmd     *exec.Cmd
	in, out *os.File
	done    chan struct{}
	once    sync.Once
	timeout time.Duration
}

func launch(cfg Config) (*process, error) {
	childIn, parentIn, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentOut, childOut, err := os.Pipe()
	if err != nil {
		childIn.Close()
		parentIn.Close()
		return nil, err
	}
	cmd := exec.Command(cfg.Executable, cfg.Args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.Workspace, childIn, childOut, cfg.Stderr
	configureProcess(cmd)
	err = cmd.Start()
	childIn.Close()
	childOut.Close()
	if err != nil {
		parentIn.Close()
		parentOut.Close()
		return nil, err
	}
	p := &process{cmd: cmd, in: parentIn, out: parentOut, done: make(chan struct{}), timeout: cfg.ShutdownTimeout}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *process) close() {
	p.once.Do(func() {
		_ = p.in.Close()
		if !wait(p.done, p.timeout) {
			terminateProcess(p.cmd)
			if !wait(p.done, p.timeout) {
				killProcess(p.cmd)
				<-p.done
			}
		}
		// Clean up any descendant go commands that survived their gopls parent.
		killProcess(p.cmd)
		_ = p.out.Close()
	})
}

func wait(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

type session struct {
	proc         *process
	client       *mcp.ClientSession
	tools        []*mcp.Tool
	instructions string
	done         chan struct{}
	sequence     atomic.Uint64
	mu           sync.Mutex
	progress     map[string]chan *mcp.ProgressNotificationParams
	once         sync.Once
}

func start(ctx context.Context, cfg Config, roots []string) (Session, error) {
	p, err := launch(cfg)
	if err != nil {
		return nil, fmt.Errorf("start gopls: %w", err)
	}
	s := &session{proc: p, done: make(chan struct{}), progress: map[string]chan *mcp.ProgressNotificationParams{}}
	// Bound initialization even if a backend never answers the handshake.
	stop := context.AfterFunc(ctx, p.close)
	defer stop()
	c := mcp.NewClient(&mcp.Implementation{Name: "gopls-mcp-proxy", Version: "0.1.0"}, &mcp.ClientOptions{
		Logger:         cfg.Logger,
		Capabilities:   &mcp.ClientCapabilities{RootsV2: &mcp.RootCapabilities{}},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			token, ok := req.Params.ProgressToken.(string)
			if !ok {
				return
			}
			s.mu.Lock()
			ch := s.progress[token]
			s.mu.Unlock()
			if ch != nil {
				copy := *req.Params
				select {
				case ch <- &copy:
				default:
				}
			}
		},
	})
	for _, root := range roots {
		c.AddRoots(&mcp.Root{URI: FileURI(root), Name: filepath.Base(root)})
	}
	cs, err := c.Connect(ctx, &mcp.IOTransport{Reader: p.out, Writer: p.in}, &mcp.ClientSessionOptions{ProtocolVersion: ProtocolVersion})
	if err != nil {
		p.close()
		return nil, fmt.Errorf("initialize gopls MCP: %w", err)
	}
	s.client = cs
	go func() { _ = cs.Wait(); close(s.done) }()
	s.instructions = cs.InitializeResult().Instructions
	// Tool lists can be paginated. Treat a repeated cursor as a backend protocol error.
	cursors := map[string]bool{}
	cursor := ""
	for {
		result, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("list gopls tools: %w", err)
		}
		s.tools = append(s.tools, result.Tools...)
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
		if cursors[cursor] {
			s.Close()
			return nil, fmt.Errorf("gopls tools/list repeated a cursor")
		}
		cursors[cursor] = true
	}
	if len(s.tools) == 0 {
		s.Close()
		return nil, fmt.Errorf("gopls returned no MCP tools")
	}
	if _, err := ToolDigest(s.tools); err != nil {
		s.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func FileURI(path string) string {
	path = filepath.ToSlash(path)
	if len(path) > 1 && path[1] == ':' {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func (s *session) Tools() []*mcp.Tool    { return s.tools }
func (s *session) Instructions() string  { return s.instructions }
func (s *session) Done() <-chan struct{} { return s.done }
func (s *session) Close()                { s.once.Do(func() { s.proc.close(); _ = s.client.Close() }) }

func (s *session) Call(ctx context.Context, params *mcp.CallToolParams, notify func(*mcp.ProgressNotificationParams)) (*mcp.CallToolResult, error) {
	p := *params
	p.Meta = maps.Clone(params.Meta)
	original := params.GetProgressToken()
	if original != nil && notify != nil {
		// Never reuse an upstream token: clients can reuse tokens across requests.
		token := fmt.Sprintf("proxy-%d", s.sequence.Add(1))
		p.SetProgressToken(token)
		ch := make(chan *mcp.ProgressNotificationParams, 32)
		s.mu.Lock()
		s.progress[token] = ch
		s.mu.Unlock()
		progressCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.progress, token); s.mu.Unlock() }()
		go func() {
			for {
				select {
				case <-progressCtx.Done():
					return
				case update := <-ch:
					if progressCtx.Err() == nil {
						update.ProgressToken = original
						notify(update)
					}
				}
			}
		}()
	}
	return s.client.CallTool(ctx, &p)
}

// ToolDigest includes full tool definitions, not just names, in deterministic order.
func ToolDigest(tools []*mcp.Tool) ([32]byte, error) {
	ordered := slices.Clone(tools)
	for _, tool := range ordered {
		if tool == nil || tool.Name == "" || tool.InputSchema == nil {
			return [32]byte{}, fmt.Errorf("invalid backend tool definition")
		}
	}
	slices.SortFunc(ordered, func(a, b *mcp.Tool) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	for i, t := range ordered {
		if i > 0 && t.Name == ordered[i-1].Name {
			return [32]byte{}, fmt.Errorf("duplicate backend tool: %s", t.Name)
		}
		b, err := json.Marshal(t.InputSchema)
		if err != nil {
			return [32]byte{}, err
		}
		var schema map[string]any
		if json.Unmarshal(b, &schema) != nil || schema["type"] != "object" {
			return [32]byte{}, fmt.Errorf("tool %s has a non-object input schema", t.Name)
		}
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
