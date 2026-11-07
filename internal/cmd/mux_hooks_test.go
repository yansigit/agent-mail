package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubRouter records the command path it saw and returns fixed results.
type stubRouter struct {
	handled bool
	err     error
	output  string // printed to stdout when handled
	calls   int
	path    []string
}

func (s *stubRouter) Route(kctx *kong.Context, _ *RunContext) (bool, error) {
	s.calls++
	s.path = selectedCommandPath(kctx)
	if s.handled {
		fmt.Print(s.output)
	}
	return s.handled, s.err
}

// resetMuxHooks restores the hook globals when the test ends. Tests touching
// them must not call t.Parallel.
func resetMuxHooks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetRouter(nil)
		extraKongOptions = nil
		programName = "olk"
	})
}

// runArgv parses argv with the MCP-side parser and runs it through
// runSelected, returning what it printed.
func runArgv(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	cli := &CLI{}
	k, err := newKongParser(cli)
	if err != nil {
		t.Fatalf("newKongParser: %v", err)
	}
	kctx, err := k.Parse(argv)
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	rc := &RunContext{Ctx: context.Background(), Flags: &cli.RootFlags}
	stdout, _, runErr := captureStdLimit(1<<20, func() error { return runSelected(kctx, rc) })
	return stdout, runErr
}

func TestRunSelected_NoRouterRunsCommand(t *testing.T) {
	resetMuxHooks(t)
	stdout, err := runArgv(t, "version", "--json")
	if err != nil {
		t.Fatalf("runSelected: %v", err)
	}
	if !strings.Contains(stdout, `"namespace"`) {
		t.Errorf("stdout = %q, want version JSON", stdout)
	}
}

func TestRunSelected_RouterHandles(t *testing.T) {
	resetMuxHooks(t)
	r := &stubRouter{handled: true, output: "routed"}
	SetRouter(r)
	stdout, err := runArgv(t, "version", "--json")
	if err != nil {
		t.Fatalf("runSelected: %v", err)
	}
	if stdout != "routed" {
		t.Errorf("stdout = %q, want only the router's output", stdout)
	}
	if r.calls != 1 {
		t.Errorf("router called %d times, want 1", r.calls)
	}
	if strings.Join(r.path, ".") != "version" {
		t.Errorf("router saw path %v, want [version]", r.path)
	}
}

func TestRunSelected_RouterDeclines(t *testing.T) {
	resetMuxHooks(t)
	r := &stubRouter{handled: false}
	SetRouter(r)
	stdout, err := runArgv(t, "version", "--json")
	if err != nil {
		t.Fatalf("runSelected: %v", err)
	}
	if !strings.Contains(stdout, `"namespace"`) {
		t.Errorf("stdout = %q, want version JSON from the upstream command", stdout)
	}
	if strings.Count(stdout, `"namespace"`) != 1 {
		t.Errorf("upstream command ran more than once: %q", stdout)
	}
	if r.calls != 1 {
		t.Errorf("router called %d times, want 1", r.calls)
	}
}

func TestRunSelected_RouterError(t *testing.T) {
	resetMuxHooks(t)
	errBoom := errors.New("boom")
	SetRouter(&stubRouter{handled: true, err: errBoom})
	stdout, err := runArgv(t, "version", "--json")
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if strings.Contains(stdout, `"namespace"`) {
		t.Errorf("upstream command ran after the router failed: %q", stdout)
	}
}

// A router error must stop the command even when the router did not claim
// it: falling through could run an unguarded upstream command (a send).
func TestRunSelected_RouterErrorNotHandled(t *testing.T) {
	resetMuxHooks(t)
	errBoom := errors.New("boom")
	SetRouter(&stubRouter{handled: false, err: errBoom})
	stdout, err := runArgv(t, "version", "--json")
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if strings.Contains(stdout, `"namespace"`) {
		t.Errorf("upstream command ran after the router failed: %q", stdout)
	}
}

type muxPingCmd struct{}

func (muxPingCmd) Run(*RunContext) error {
	fmt.Print("pong")
	return nil
}

type muxExtra struct {
	MuxPing muxPingCmd `cmd:"" name:"mux-ping" help:"Test-only command"`
}

func TestRegisterKongOptions_AddsCommand(t *testing.T) {
	resetMuxHooks(t)
	RegisterKongOptions(kong.Embed(&muxExtra{}))
	stdout, err := runArgv(t, "mux-ping")
	if err != nil {
		t.Fatalf("runSelected: %v", err)
	}
	if stdout != "pong" {
		t.Errorf("stdout = %q, want pong", stdout)
	}
}

func TestMuxE2E_RouterServesMCPCall(t *testing.T) {
	resetMuxHooks(t)
	SetRouter(&stubRouter{handled: true, output: "routed"})
	cs := connectE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "version", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool(version): %v", err)
	}
	if res.IsError {
		t.Fatalf("version call reported error: %q", resultText(res))
	}
	if got := strings.TrimSpace(resultText(res)); got != "routed" {
		t.Errorf("tool result = %q, want the router's output", got)
	}
}

// helpText returns the root --help output of the MCP-side parser.
func helpText(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	RegisterKongOptions(kong.Writers(&buf, &buf))
	k, err := newKongParser(&CLI{})
	if err != nil {
		t.Fatalf("newKongParser: %v", err)
	}
	_, _ = k.Parse([]string{"--help"}) // the no-op Exit lets Parse return
	return buf.String()
}

func mcpServerName(t *testing.T) string {
	t.Helper()
	return connectE2E(t).InitializeResult().ServerInfo.Name
}

func TestBranding_DefaultIsOlk(t *testing.T) {
	resetMuxHooks(t)
	stdout, err := runArgv(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(stdout, "olk ") {
		t.Errorf("version = %q, want it to start with \"olk \"", stdout)
	}
	if help := helpText(t); !strings.Contains(help, "Usage: olk") {
		t.Errorf("help does not name olk:\n%s", help)
	}
	if name := mcpServerName(t); name != "olk" {
		t.Errorf("MCP server name = %q, want olk", name)
	}
}

func TestBranding_SetBrandingRenames(t *testing.T) {
	resetMuxHooks(t)
	SetBranding(Branding{Name: "mailmux", Description: "Mail for agents"})
	stdout, err := runArgv(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(stdout, "mailmux ") {
		t.Errorf("version = %q, want it to start with \"mailmux \"", stdout)
	}
	help := helpText(t)
	if !strings.Contains(help, "Usage: mailmux") || !strings.Contains(help, "Mail for agents") {
		t.Errorf("help does not show the mailmux branding:\n%s", help)
	}
	if name := mcpServerName(t); name != "mailmux" {
		t.Errorf("MCP server name = %q, want mailmux", name)
	}
}
