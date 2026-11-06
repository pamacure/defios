package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/plugin"
	"defios.com/internal/secret"
)

// DefaultMCPCallTimeout bounds an MCP Server's start and each call of one
// of its tools, unless Options.MCPCallTimeout says otherwise.
const DefaultMCPCallTimeout = 60 * time.Second

// errMCPStopped is an MCP Server asked for once the serve is stopping.
var errMCPStopped = errors.New("serve: stopping: MCP Servers no longer start")

// toolNamePattern is what every provider allows as a tool's name.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// mcpTools are the tools of the MCP Servers grants grants handle, sorted by
// name, each offered as <plugin>__<server>__<tool> (ADR 0030). Each server
// starts now if it does not run yet, with PATH, HOME and the Secrets its
// grant names, nothing else; one that fails to start, a grant naming no
// installed server or a Secret it does not ask for, and a tool name a
// provider would refuse each fail the summon, naming handle.
func (s *server) mcpTools(ctx context.Context, handle string, grants map[string]config.MCPGrant) ([]agent.Tool, error) {
	r := &secret.Resolver{Group: s.opts.Alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
	var out []agent.Tool
	for _, ref := range slices.Sorted(maps.Keys(grants)) {
		fail := func(format string, a ...any) ([]agent.Tool, error) {
			return nil, fmt.Errorf("@%s's profile: mcp: %q: "+format, append([]any{handle, ref}, a...)...)
		}
		pname, sname, err := plugin.ParseServerRef(ref)
		if err != nil {
			return nil, fmt.Errorf("@%s's profile: mcp: %w", handle, err)
		}
		p, err := plugin.Find(s.opts.Paths.PluginsDir(), pname)
		if errors.Is(err, plugin.ErrNotFound) {
			return fail("no Plugin %q is installed", pname)
		} else if err != nil {
			return fail("%w", err)
		}
		srv, ok := p.Servers[sname]
		if !ok {
			return fail("Plugin %s has no MCP Server %q", pname, sname)
		}
		names := slices.Sorted(slices.Values(grants[ref].Secrets))
		if err := srv.CheckSecrets(names); err != nil {
			return fail("%w", err)
		}
		var env []string
		for _, k := range []string{"PATH", "HOME"} {
			if v := s.opts.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
		for _, name := range names {
			v, err := r.Resolve("$" + name)
			if err != nil {
				return fail("secrets: %w", err)
			}
			env = append(env, name+"="+v)
		}
		proc := s.mcp.proc(ref+"\x00"+strings.Join(names, ","), ref, func() (*exec.Cmd, error) {
			return serverCommand(p, sname, srv, env)
		})
		if proc == nil {
			return nil, errMCPStopped
		}
		startCtx, cancel := context.WithTimeout(ctx, s.mcpTimeout())
		_, _, tools, err := proc.live(startCtx)
		cancel()
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			name := pname + "__" + sname + "__" + t.Name
			if !toolNamePattern.MatchString(name) {
				return fail("its tool %q would be offered as %q, which is not 1–64 characters of [a-zA-Z0-9_-]", t.Name, name)
			}
			schema := json.RawMessage(`{"type":"object"}`)
			if t.InputSchema != nil {
				if schema, err = json.Marshal(t.InputSchema); err != nil {
					return fail("tool %q: input schema: %w", t.Name, err)
				}
			}
			desc := t.Description
			if desc == "" {
				desc = fmt.Sprintf("The tool %s of MCP Server %s.", t.Name, ref)
			}
			out = append(out, agent.Tool{Name: name, Description: desc, Schema: schema, Call: proc.call(t.Name, s.mcpTimeout())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mcpTimeout is the bound on an MCP Server's start and each call.
func (s *server) mcpTimeout() time.Duration {
	if s.opts.MCPCallTimeout > 0 {
		return s.opts.MCPCallTimeout
	}
	return DefaultMCPCallTimeout
}

// serverCommand is the process that runs p's MCP Server name, srv, with
// exactly env: an installed Plugin's command in its directory, or for a
// built-in this binary itself as `defios mcp <plugin>/<server>`.
func serverCommand(p plugin.Plugin, name string, srv plugin.Server, env []string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if p.BuiltIn {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("serve: finding this binary for built-in MCP Server %s/%s: %w", p.Name, name, err)
		}
		cmd = exec.Command(exe, "mcp", p.Name+"/"+name)
	} else {
		cmd = exec.Command(srv.Command, srv.Args...)
		cmd.Dir = p.Dir
	}
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = []string{} // nil would be the serve's own environment
	}
	return cmd, nil
}

// newMCPServers is a serve's MCP Server processes, none running yet.
func newMCPServers(logf func(format string, args ...any)) *mcpServers {
	ctx, cancel := context.WithCancel(context.Background())
	return &mcpServers{logf: logf, stopping: ctx, stop: cancel}
}

// mcpServers are the MCP Server processes this serve runs, one per server
// and set of granted Secrets, shared by every agent that grants it so,
// until the serve stops (ADR 0030).
type mcpServers struct {
	logf func(format string, args ...any)
	// stopping ends when the serve stops, cutting any start in progress so
	// close is not held up by a server that never answers.
	stopping context.Context
	stop     context.CancelFunc
	mu       sync.Mutex
	procs    map[string]*mcpProc
	closed   bool
}

// proc is the process keyed key, for the server ref, which cmd starts when
// it next starts; nil once the serve is stopping.
func (m *mcpServers) proc(key, ref string, cmd func() (*exec.Cmd, error)) *mcpProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	if m.procs == nil {
		m.procs = map[string]*mcpProc{}
	}
	p, ok := m.procs[key]
	if !ok {
		p = &mcpProc{ref: ref, logf: m.logf, stopping: m.stopping}
		m.procs[key] = p
	}
	p.mu.Lock()
	p.cmd = cmd // a Secret's value set since starts with the next start
	p.mu.Unlock()
	return p
}

// close stops every process and starts no more.
func (m *mcpServers) close() {
	m.mu.Lock()
	m.closed = true
	m.stop()
	procs := slices.Collect(maps.Values(m.procs))
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(p.stop)
	}
	wg.Wait()
}

// mcpProc is one MCP Server process, started when first needed and again
// whenever it is found to have ended.
type mcpProc struct {
	ref      string
	logf     func(format string, args ...any)
	stopping context.Context // ends when the serve stops

	mu      sync.Mutex
	cmd     func() (*exec.Cmd, error)
	session *mcp.ClientSession
	ended   chan struct{} // closed once session ends
	tools   []*mcp.Tool
	stopped bool
}

// live is the running session, closed once it ends, and its tools,
// starting the process if it does not run.
func (p *mcpProc) live(ctx context.Context) (*mcp.ClientSession, <-chan struct{}, []*mcp.Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil, nil, nil, errMCPStopped
	}
	if p.session != nil {
		select {
		case <-p.ended:
			p.logf("serve: MCP Server %s ended; starting it again", p.ref)
			p.session.Close()
			p.session = nil
		default:
			return p.session, p.ended, p.tools, nil
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(p.stopping, cancel)()
	if err := p.start(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("MCP Server %s did not start: %w", p.ref, err)
	}
	return p.session, p.ended, p.tools, nil
}

// start starts the process and lists its tools; p.mu is held.
func (p *mcpProc) start(ctx context.Context) error {
	cmd, err := p.cmd()
	if err != nil {
		return err
	}
	cmd.Stderr = &lineLog{prefix: "serve: MCP Server " + p.ref + ": ", logf: p.logf}
	client := mcp.NewClient(&mcp.Implementation{Name: "defios", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	var tools []*mcp.Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			cs.Close()
			return fmt.Errorf("listing its tools: %w", err)
		}
		tools = append(tools, t)
	}
	ended := make(chan struct{})
	go func() { cs.Wait(); close(ended) }()
	p.session, p.ended, p.tools = cs, ended, tools
	return nil
}

// stop ends the process, if it runs, for good.
func (p *mcpProc) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.session != nil {
		p.session.Close()
		p.session = nil
	}
}

// call is the Call of p's tool name: its text result, or as an error an
// MCP error or the server's failing to answer within timeout. A process
// found ended is started again first.
func (p *mcpProc) call(name string, timeout time.Duration) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cs, ended, _, err := p.live(ctx)
		if err != nil {
			return "", err
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: input})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("MCP Server %s did not answer within %s", p.ref, timeout)
		}
		if errors.Is(err, mcp.ErrConnectionClosed) {
			// The process is gone: once its session has ended too, the
			// next use starts it again rather than finding it still open.
			select {
			case <-ended:
			case <-ctx.Done():
			}
		}
		if err != nil {
			return "", fmt.Errorf("MCP Server %s: %w", p.ref, err)
		}
		text := resultText(res)
		if res.IsError {
			return "", errors.New(text)
		}
		return text, nil
	}
}

// resultText is a tool result's text content, one block a paragraph; with
// none, its structured content as JSON.
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	omitted := 0
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		} else {
			omitted++
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("(%d non-text content blocks omitted)", omitted))
	}
	return strings.Join(parts, "\n\n")
}

// lineLog logs what a process writes, a line at a time.
type lineLog struct {
	prefix string
	logf   func(format string, args ...any)
	buf    []byte
}

// maxLogLine is where a line with no newline yet is logged anyway.
const maxLogLine = 4096

func (l *lineLog) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			if len(l.buf) < maxLogLine {
				return len(b), nil
			}
			i = len(l.buf)
		}
		l.logf("%s%s", l.prefix, l.buf[:i])
		l.buf = l.buf[min(i+1, len(l.buf)):]
	}
}
