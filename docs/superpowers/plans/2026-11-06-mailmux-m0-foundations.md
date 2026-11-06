# mailmux M0 (Foundations) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a `mailmux` binary that behaves exactly like olk under its own name, namespace and environment variables, with the hook points later milestones plug into, while plain `olk` stays byte-for-byte unchanged in behaviour.

**Architecture:** A new main package `cmd/mailmux` configures branding, namespace and environment, then calls upstream's `cmd.Execute()`. A fork-owned file `internal/cmd/mux_hooks.go` holds a router hook (`runSelected`), extra kong options and branding; upstream files change only at the call sites listed below.

**Tech Stack:** Go 1.26.4, `github.com/alecthomas/kong` v1.15.0, `github.com/modelcontextprotocol/go-sdk` v1.4.1, golangci-lint 2.13.2, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-11-06-mailmux-multi-provider-design.md` (sections 4.1, 4.4, 12 row M0)

## Global Constraints

- Upstream files may change **only** at these points: `internal/cmd/root.go` (kong options, `runSelected`), `internal/cmd/mcp_invoke.go` (`runSelected`), `internal/cmd/mcp_server.go` (`newKongParser` options, server implementation name), `internal/cmd/version.go` (program name), `internal/config/config.go` (`DefaultClientID` const → var), `CLAUDE.md` (one pointer line). Every other change goes in new files.
- New files inside upstream packages are named `mux_*.go` / `mux_*_test.go`.
- Default behaviour of `cmd/olk` is unchanged: program name `olk`, namespace `olk` (`olk-dev` under `make build`), no router, no extra kong options.
- `mailmux` namespace: `mailmux` by default (no build flags needed); `mailmux-dev` for `make -f Makefile.mailmux build`.
- `mailmux` ignores every inherited `OLK_*` environment variable and maps each `MAILMUX_<X>` to `OLK_<X>`.
- The Go module path stays `github.com/rlrghb/olkcli`; imports use it; `goimports` local prefix `github.com/rlrghb/olkcli`.
- Do not use `t.Parallel()` in tests that touch the hook globals (no test in `internal/cmd` uses it today).
- Running as root: `TestMailAttachmentsSaveReportsAWriteFailure` fails on `main`. It is fixed on branch `fix/skip-readonly-dir-test-as-root`; merge that branch into the work branch before Task 1, or treat that single failure as known.
- Commit messages end with the session's attribution lines.

## Review Focus

1. **`go install …/cmd/mailmux` with no build flags** must use namespace `mailmux`, never olk's keychain or config. Pinned in Task 4 (`TestConfigure_DefaultNamespaceIsMailmux`).
2. **Leftover olk variables** (`OLK_ACCOUNT`, `OLK_CONFIG_DIR`, `OLK_KEYRING_PASSWORD` …) in the user's shell must not leak into mailmux, and `MAILMUX_*` must win regardless of order. Pinned in Task 4 (`TestIsolateEnv_*`).
3. **A router that declines** must leave the upstream command running exactly once; a router error must be returned unchanged and the command must not run. Pinned in Task 2.
4. **MCP tool calls** must go through the same router as the CLI. Pinned in Task 2 (`TestMuxE2E_RouterServesMCPCall`).
5. **`olk` itself** must still say `olk` everywhere (help, version, MCP server name) after the branding hooks land. Pinned in Task 3 (`TestBranding_DefaultIsOlk`).

---

### Task 1: Confirm embedded result structs keep the output contract

Spec 4.4 risk: mailmux results embed upstream result structs. `transformUntrusted` (`internal/outfmt/untrusted.go:89-107`) recurses into struct-kind fields, which includes embedded ones; this task pins that with a test. No production code.

**Files:**
- Create: `internal/outfmt/mux_embedded_test.go`

**Interfaces:**
- Consumes: `wrapUntrusted(v interface{}, id string) interface{}` (package-internal, `internal/outfmt/untrusted.go:56`).
- Produces: nothing.

- [ ] **Step 1: Write the tests**

```go
package outfmt

type muxInner struct {
	ID      string `json:"id"`
	Subject string `json:"subject" untrusted:"true"`
}

type muxByValue struct {
	muxInner
	Account string `json:"account"`
}

type muxByPointer struct {
	*muxInner
	Account string `json:"account"`
}

func TestMuxEmbedded_WrapsEmbeddedUntrustedFields(t *testing.T)
// in := muxByValue{muxInner{ID: "1", Subject: "hi"}, "a@x"}
// out := wrapUntrusted(in, "abcd").(muxByValue)
// out.Subject == "[UNTRUSTED:abcd]hi[/UNTRUSTED:abcd]"; out.ID == "1"; out.Account == "a@x"
// in.Subject == "hi" (original not mutated)

func TestMuxEmbedded_WrapsPointerEmbeddedFields(t *testing.T)
// same assertions with muxByPointer{&muxInner{...}, "a@x"}; in.muxInner.Subject still "hi"

func TestMuxEmbedded_JSONIsFlat(t *testing.T)
// json.Marshal(muxByValue{...}) == `{"id":"1","subject":"hi","account":"a@x"}`
```

- [ ] **Step 2: Run them**

Run: `go test -race -count=1 -run TestMuxEmbedded ./internal/outfmt/`
Expected: PASS. If `TestMuxEmbedded_Wraps*` fails, stop and report: spec 4.4's fallback (explicit wrapper fields instead of embedding) must then be chosen before M1.

- [ ] **Step 3: Commit**

```bash
git add internal/outfmt/mux_embedded_test.go
git commit -m "test(outfmt): pin untrusted wrapping and JSON shape for embedded structs"
```

---

### Task 2: Router hook and extra kong options

**Files:**
- Create: `internal/cmd/mux_hooks.go`
- Create: `internal/cmd/mux_hooks_test.go`
- Modify: `internal/cmd/root.go` — the `kong.Parse(cli, …)` call in `Execute()` (line 253) and `err := ctx.Run(runCtx)` (line 297)
- Modify: `internal/cmd/mcp_server.go` — `newKongParser` (line 356)
- Modify: `internal/cmd/mcp_invoke.go` — `return kctx.Run(runCtx)` inside the `captureStdLimit` closure (line 81)

**Interfaces:**
- Consumes: `newKongParser(cli *CLI) (*kong.Kong, error)`, `captureStdLimit(limit int, fn func() error) (string, string, error)`, `connectE2E(t, writes...)`, `resultText(res)`, `selectedCommandPath(kctx) []string`.
- Produces (in `mux_hooks.go`):
  - `type Router interface { Route(kctx *kong.Context, rc *RunContext) (handled bool, err error) }`
  - `func SetRouter(r Router)` — `nil` restores default behaviour.
  - `func RegisterKongOptions(opts ...kong.Option)` — appended after upstream's options on every parser (CLI and MCP).
  - `func withExtraOptions(base ...kong.Option) []kong.Option` — returns `base` followed by the registered options.
  - `func runSelected(kctx *kong.Context, rc *RunContext) error` — calls `router.Route` when set; if it returns `handled == true`, returns its error; otherwise returns `kctx.Run(rc)`.

- [ ] **Step 1: Write the failing tests** in `mux_hooks_test.go`

```go
// stubRouter records the command path it saw and returns fixed results.
type stubRouter struct {
	handled bool
	err     error
	output  string // printed to stdout when handled
	calls   int
	path    []string
}

func resetMuxHooks(t *testing.T) // t.Cleanup: SetRouter(nil); extraKongOptions = nil

func TestRunSelected_NoRouterRunsCommand(t *testing.T)
// parse ["version", "--json"] with newKongParser(&CLI{}); captureStdLimit(1<<20, runSelected)
// stdout contains `"namespace"`; err == nil

func TestRunSelected_RouterHandles(t *testing.T)
// router{handled: true, output: "routed"}; stdout == "routed"; does not contain `"namespace"`;
// router.calls == 1; router.path == []string{"version"}

func TestRunSelected_RouterDeclines(t *testing.T)
// router{handled: false}; stdout contains `"namespace"`; router.calls == 1

func TestRunSelected_RouterError(t *testing.T)
// errBoom := errors.New("boom"); router{handled: true, err: errBoom}
// errors.Is(err, errBoom); stdout does not contain `"namespace"`

func TestRegisterKongOptions_AddsCommand(t *testing.T)
// type muxPingCmd struct{}; func (muxPingCmd) Run(*RunContext) error { fmt.Print("pong"); return nil }
// type muxExtra struct { MuxPing muxPingCmd `cmd:"" name:"mux-ping"` }
// RegisterKongOptions(kong.Embed(&muxExtra{})); parse ["mux-ping"] via newKongParser; runSelected prints "pong"

func TestMuxE2E_RouterServesMCPCall(t *testing.T)
// SetRouter(&stubRouter{handled: true, output: "routed"}); cs := connectE2E(t)
// CallTool("version") → !res.IsError; strings.TrimSpace(resultText(res)) == "routed"
```

The stub's `Route` sets `path = selectedCommandPath(kctx)`, increments `calls`, and prints `output` with `fmt.Print` when `handled`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 -run 'TestRunSelected|TestRegisterKongOptions|TestMuxE2E' ./internal/cmd/`
Expected: build failure, `undefined: runSelected` / `SetRouter` / `RegisterKongOptions`.

- [ ] **Step 3: Implement `mux_hooks.go`** with the `Produces` symbols above. Package-level `var router Router` and `var extraKongOptions []kong.Option`. Comment at the top of the file: fork-owned; upstream reaches it only through `runSelected` and `withExtraOptions`.

- [ ] **Step 4: Wire the call sites**
  - `root.go`: `kong.Parse(cli, withExtraOptions(<existing options unchanged>)...)` and `err := runSelected(ctx, runCtx)`.
  - `mcp_server.go`: `kong.New(cli, withExtraOptions(<existing options unchanged>)...)`.
  - `mcp_invoke.go`: `return runSelected(kctx, runCtx)`.

- [ ] **Step 5: Run the new tests, then the package**

Run: `go test -race -count=1 -run 'TestRunSelected|TestRegisterKongOptions|TestMuxE2E' ./internal/cmd/ && go test -race -count=1 ./internal/cmd/`
Expected: PASS (all upstream tests included).

- [ ] **Step 6: Commit**

```bash
git add internal/cmd/mux_hooks.go internal/cmd/mux_hooks_test.go internal/cmd/root.go internal/cmd/mcp_server.go internal/cmd/mcp_invoke.go
git commit -m "feat(cmd): add router hook and extra kong options for mailmux"
```

---

### Task 3: Branding hook

**Files:**
- Modify: `internal/cmd/mux_hooks.go`, `internal/cmd/mux_hooks_test.go`
- Modify: `internal/cmd/version.go:65` — the `fmt.Printf("olk %s …")` line
- Modify: `internal/cmd/mcp_server.go` — `mcp.NewServer(&mcp.Implementation{Name: "olk", …})` in `buildMCPServer` (line 375)

**Interfaces:**
- Consumes: `RegisterKongOptions` (Task 2).
- Produces:
  - `type Branding struct { Name, Description string }`
  - `func SetBranding(b Branding)` — sets `programName = b.Name` and calls `RegisterKongOptions(kong.Name(b.Name), kong.Description(b.Description))`. kong applies `Name` after building the model, so a later `kong.Name` overrides upstream's.
  - `var programName = "olk"` — used by `version.go` and `buildMCPServer`.
  - Test helper `resetMuxHooks` (Task 2) also restores `programName = "olk"`.

- [ ] **Step 1: Write the failing tests**

```go
func TestBranding_DefaultIsOlk(t *testing.T)
// version (plain) stdout starts with "olk "; --help (kong.Writers(&buf,&buf) registered) contains "Usage: olk";
// connectE2E(t).InitializeResult().ServerInfo.Name == "olk"

func TestBranding_SetBrandingRenames(t *testing.T)
// SetBranding(Branding{Name: "mailmux", Description: "Mail for agents"})
// version stdout starts with "mailmux "; help contains "Usage: mailmux" and "Mail for agents";
// connectE2E(t).InitializeResult().ServerInfo.Name == "mailmux"
```

- [ ] **Step 2: Run to verify `TestBranding_SetBrandingRenames` fails**

Run: `go test -count=1 -run TestBranding ./internal/cmd/`
Expected: build failure `undefined: SetBranding`.

- [ ] **Step 3: Implement** `Branding`, `SetBranding`, `programName`; replace the literal `olk` in `version.go:65` with `programName` (`fmt.Printf("%s %s (commit: %s, built: %s)\n", programName, Version, Commit, Date)`) and `Name: programName` in `buildMCPServer`.

- [ ] **Step 4: Run the package**

Run: `go test -race -count=1 ./internal/cmd/`
Expected: PASS, including `version_test.go`.

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/mux_hooks.go internal/cmd/mux_hooks_test.go internal/cmd/version.go internal/cmd/mcp_server.go
git commit -m "feat(cmd): let a build rename the program in help, version and MCP"
```

---

### Task 4: `cmd/mailmux` entry point

**Files:**
- Create: `cmd/mailmux/main.go`, `cmd/mailmux/setup.go`, `cmd/mailmux/setup_test.go`
- Modify: `internal/config/config.go:26-29` — move `DefaultClientID` out of the `const` block into `var DefaultClientID = "51e726d0-22a4-45f7-a71c-b472ff84c027"`; `DefaultTenantID` stays a const.
- Modify: spec section 4.1 item 5 is already updated in this plan's commit (no action).

**Interfaces:**
- Consumes: `cmd.SetBranding`, `cmd.Branding` (Task 3), `cmd.Execute() int`, `config.Namespace`, `config.DefaultClientID`, `config.ConfigDir()`.
- Produces (package `main`):
  - `var namespace = "mailmux"` and `var clientID = ""` — overridable with `-X main.namespace=…` / `-X main.clientID=…`.
  - `func isolateEnv(environ []string, setenv func(key, value string) error, unsetenv func(key string) error) error` — unsets every `OLK_*` key, then for every `MAILMUX_<X>` with non-empty `<X>` sets `OLK_<X>` to its value (split each entry on the first `=`).
  - `func configure()` — `config.Namespace = namespace`; if `clientID != ""`, `config.DefaultClientID = clientID`; `cmd.SetBranding(cmd.Branding{Name: "mailmux", Description: "Mail for agents across Outlook, Gmail and IMAP"})`.
  - `main()` — `isolateEnv(os.Environ(), os.Setenv, os.Unsetenv)` (on error print `mailmux: <err>` to stderr, exit 1), `configure()`, `os.Exit(cmd.Execute())`.

- [ ] **Step 1: Write the failing tests** in `setup_test.go` (use a `map[string]string` fake environment for `setenv`/`unsetenv`)

```go
func TestIsolateEnv_DropsOlkVariables(t *testing.T)
// env: OLK_ACCOUNT=a, OLK_CONFIG_DIR=/olk, HOME=/h → result: HOME=/h only

func TestIsolateEnv_MapsMailmuxVariables(t *testing.T)
// env: MAILMUX_ACCOUNT=b, MAILMUX_NO_SEND=1 → OLK_ACCOUNT=b, OLK_NO_SEND=1 (MAILMUX_* kept)

func TestIsolateEnv_MailmuxWinsInEitherOrder(t *testing.T)
// [OLK_ACCOUNT=a, MAILMUX_ACCOUNT=b] and [MAILMUX_ACCOUNT=b, OLK_ACCOUNT=a] → OLK_ACCOUNT=b both times

func TestIsolateEnv_KeepsEqualsInValues(t *testing.T)
// MAILMUX_SELECT=a=b → OLK_SELECT="a=b"

func TestIsolateEnv_IgnoresBarePrefix(t *testing.T)
// MAILMUX_=x → no OLK_ key set

func TestConfigure_DefaultNamespaceIsMailmux(t *testing.T)
// t.Setenv("OLK_CONFIG_DIR", ""); configure(); config.Namespace == "mailmux";
// filepath.Base(config.ConfigDir()) == "mailmux"; config.DefaultClientID unchanged (clientID empty)
// restore config.Namespace / DefaultClientID in t.Cleanup

func TestConfigure_ClientIDOverride(t *testing.T)
// clientID = "11111111-2222-3333-4444-555555555555"; configure(); config.DefaultClientID equals it
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -count=1 ./cmd/mailmux/`
Expected: build failure (`undefined: isolateEnv`, `configure`; assigning to `config.DefaultClientID` fails while it is a const).

- [ ] **Step 3: Implement** `setup.go`, `main.go`, and the `config.go` const → var change.

- [ ] **Step 4: Run the tests and a real binary**

Run:
```bash
go test -race -count=1 ./cmd/mailmux/ ./internal/config/
go build -o bin/mailmux ./cmd/mailmux
MAILMUX_CONFIG_DIR="$(mktemp -d)" ./bin/mailmux version --json
./bin/mailmux --help | head -3
OLK_ACCOUNT=leak@example.com MAILMUX_CONFIG_DIR="$(mktemp -d)" ./bin/mailmux whoami --json
go build -o bin/olk ./cmd/olk && ./bin/olk version
```
Expected: version JSON has `"namespace":"mailmux"`; help shows `Usage: mailmux`; `whoami` fails with "no account configured" (proves `OLK_ACCOUNT` was dropped); `olk version` starts with `olk `.

- [ ] **Step 5: Commit**

```bash
git add cmd/mailmux internal/config/config.go
git commit -m "feat: add the mailmux entry point with its own namespace and env"
```

---

### Task 5: Build file, CI check and fork rules

**Files:**
- Create: `Makefile.mailmux`
- Create: `.github/workflows/mailmux.yml`
- Create: `docs/mailmux/development.md`
- Modify: `CLAUDE.md` — one line under the title: `> **mailmux fork:** read docs/mailmux/development.md before changing files outside internal/mux, cmd/mailmux or mux_*.go.`

**Interfaces:**
- Consumes: `main.namespace`, `main.clientID` (Task 4); version ldflags `internal/cmd.Version|Commit|Date` (as in `Makefile`).
- Produces: `make -f Makefile.mailmux build` → `./bin/mailmux` (namespace `mailmux-dev`); `install` → `$(go env GOPATH)/bin/mailmux` (namespace `mailmux`); `test` and `lint` run the same commands as `Makefile`. `MAILMUX_CLIENT_ID` (make variable, empty by default) feeds `-X main.clientID`.

- [ ] **Step 1: Write `Makefile.mailmux`** with targets `build`, `install`, `test`, `lint`, `clean`, mirroring `Makefile`'s `VERSION`/`COMMIT`/`DATE` ldflags plus `-X main.namespace=$(NAMESPACE)` (default `mailmux-dev` for build) and `-X main.clientID=$(MAILMUX_CLIENT_ID)`.

- [ ] **Step 2: Verify it**

Run: `make -f Makefile.mailmux build && ./bin/mailmux version --json`
Expected: `"namespace":"mailmux-dev"`.

- [ ] **Step 3: Write `.github/workflows/mailmux.yml`**: on `push` and `pull_request`; job matrix `ubuntu-latest`, `macos-latest`; steps: checkout and setup-go pinned to the same SHAs as `.github/workflows/ci.yml` (`actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10`, `actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c`, `go-version-file: go.mod`); `make -f Makefile.mailmux build`; `./bin/mailmux version --json | grep -q '"namespace":"mailmux-dev"'`; `go test -race -count=1 ./cmd/mailmux/`. Permissions `contents: read`.

- [ ] **Step 4: Write `docs/mailmux/development.md`** covering, in this order:
  1. The rule: new code in `internal/mux/…`, `cmd/mailmux`, or `mux_*.go` files; upstream files only at the hook points in this plan's Global Constraints (keep that list here and update it when a milestone adds a hook).
  2. Namespaces: `olk` / `olk-dev` (upstream builds) vs `mailmux` / `mailmux-dev`.
  3. Environment: mailmux drops `OLK_*` and maps `MAILMUX_*`; olk is unaffected.
  4. Merging upstream: `git remote add upstream https://github.com/rlrghb/olkcli.git`, `git fetch upstream`, `git merge upstream/main`, then `make test` and `make -f Makefile.mailmux build`; conflicts should only appear at hook lines.
  5. The known limitation: about 19 user-facing upstream messages still say `olk …` (for example "Run 'olk auth login' first"); they are not rewritten in M0.

- [ ] **Step 5: Full verification**

Run: `make test && make lint && make -f Makefile.mailmux build`
Expected: all tests pass (see Global Constraints about the root-only test); lint reports no issues.

- [ ] **Step 6: Commit and push**

```bash
git add Makefile.mailmux .github/workflows/mailmux.yml docs/mailmux/development.md CLAUDE.md
git commit -m "build: add mailmux build file, CI check and fork development rules"
git push -u origin <work-branch>
```
