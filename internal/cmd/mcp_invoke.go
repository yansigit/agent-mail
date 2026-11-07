package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/kong"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// makeHandler returns the MCP tool handler for a single command binding. Each
// call reconstructs an argv, re-parses it through a fresh kong instance, runs
// the matched command against a per-call RunContext, and returns whatever the
// command wrote to stdout/stderr.
func makeHandler(b *toolBinding) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if req.Params != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
			}
		}

		// Reject arguments the tool doesn't declare (fixed-schema contract) so a
		// model can't smuggle in unvetted flags.
		if err := rejectUnknownArgs(b, args); err != nil {
			return errorResult(err.Error()), nil
		}

		argv, err := buildArgv(b, args)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		// The deadline covers waiting for a turn as well as running, so a call
		// queued behind a stuck one gives up rather than waiting forever.
		callCtx, cancel := context.WithTimeout(ctx, callTimeout(b.env.timeout))
		defer cancel()

		// Calls run one at a time, parser construction included: kong reads
		// os.Stdout and os.Stderr, which a running call has swapped.
		release, err := acquireCallSlot(callCtx)
		if err != nil {
			return errorResult(fmt.Sprintf("not run: %v while waiting for another tool call", err)), nil
		}
		defer release()

		cli, kctx, err := prepareCall(argv, &b.env)
		if err != nil {
			var bad *argvError
			if errors.As(err, &bad) {
				return errorResult(fmt.Sprintf("parse error: %v", bad.Unwrap())), nil
			}
			return nil, err
		}
		if err := callCtx.Err(); err != nil {
			return errorResult(fmt.Sprintf("not run: %v", err)), nil
		}

		// A call's local writes are always confined; a binding without a
		// scope refuses them all.
		saves := b.env.saves
		if saves == nil {
			saves = &saveScope{refusal: "no --download-dir is set"}
		}
		runCtx := &RunContext{Ctx: callCtx, Flags: &cli.RootFlags, saves: saves, stdinIsProtocol: true}

		limit := b.maxOutputBytes
		if limit <= 0 {
			limit = defaultMaxOutputBytes
		}
		stdout, stderr, runErr := captureStdLimit(limit, func() error {
			return runSelected(kctx, runCtx)
		})
		// Mark truncation on each stream as captured, before any trimming
		// could bring it back under the cap and hide the cut.
		stdout, stderr = capText(stdout, limit), capText(stderr, limit)

		if runErr != nil {
			return failedResult(stdout, stderr, runErr, limit), nil
		}

		text := stdout
		if s := strings.TrimSpace(stderr); s != "" {
			text = strings.TrimRight(stdout, "\n") + "\n[stderr]\n" + s
		}
		text = capText(text, limit)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil
	}
}

// prepareCall turns a rebuilt argv into the CLI a tool call runs under: parse it
// fresh, restore the operator's launch-time globals over the top, then force the
// two settings no call may choose for itself.
//
// It exists as its own function so a test can assert what a call actually runs
// with. The alternative — reading it back out of a completed call — needs live
// credentials for any command that touches Graph, which is every command whose
// launch flags matter.
//
// A rejected argv comes back wrapped in argvError, which is what separates the
// agent's mistake from the server's: the former is a tool result the model can
// learn from, the latter fails the call outright.
func prepareCall(argv []string, env *callEnv) (*CLI, *kong.Context, error) {
	cli := &CLI{}
	k, err := newKongParser(cli)
	if err != nil {
		return nil, nil, fmt.Errorf("building parser: %w", err)
	}
	kctx, err := k.Parse(argv)
	if err != nil {
		return nil, nil, &argvError{err: err}
	}

	applyLaunchEnv(cli, env)

	// Force agent-safe output regardless of what the reparsed argv set: never
	// block on a prompt, and wrap externally-controlled free text so the model
	// treats it as data, not instructions.
	cli.NoInput = true
	cli.WrapUntrusted = true

	return cli, kctx, nil
}

// argvError marks a failure to parse the argv a tool call was rebuilt into. It
// is the agent's input that was wrong, so the caller reports it back rather than
// treating it as the server breaking.
type argvError struct{ err error }

func (e *argvError) Error() string { return e.err.Error() }
func (e *argvError) Unwrap() error { return e.err }

// applyLaunchEnv restores the operator's launch-time globals onto a per-call CLI.
//
// The rebuilt argv carries only the tool's own arguments, so without this a
// server started with --account or --timeout quietly ignored them. Each field is
// named explicitly rather than copying the whole flag struct, so adding a global
// flag never grants it silent passage into tool calls.
//
// Mailbox is absent here on purpose: buildArgv resolves it, because only there is
// a permitted per-call choice distinguishable from the launch default.
// The launch values are a complete snapshot: kong applied flag over environment
// over default when the server started, so an empty or false one means the
// operator's command line resolved to exactly that. Each is therefore restored
// unconditionally rather than only when set.
//
// Restoring conditionally looks safer and is not. No tool offers an account, a
// timeout or a mailbox, so a value surviving the reparse can only have come from
// an ambient OLK_* variable, which kong re-reads every time. Skipping the empty
// case lets that variable through: a server started with `--account=` against an
// ambient OLK_ACCOUNT ran every call as the ambient identity, and `--verbose=false`
// against an ambient OLK_VERBOSE logged anyway. The guards matter most here,
// because registration already filtered the tool list using the launch values —
// restoring anything else would leave enforcement disagreeing with what was
// advertised.
//
// --concise is the one exception. It is the only one of these a tool call can
// legitimately set for itself, through the synthetic argument injected into every
// read tool's schema, so the launch value acts as a floor rather than a
// replacement.
func applyLaunchEnv(cli *CLI, env *callEnv) {
	cli.Account = env.account
	cli.Timeout = env.timeout
	cli.TimeZone = env.timezone
	cli.Select = env.selectFields
	cli.ImmutableIDs = env.immutableIDs
	cli.Verbose = env.verbose
	cli.ResultsOnly = env.resultsOnly
	cli.DryRun = env.dryRun
	cli.NoWrite = env.noWrite
	cli.NoSend = env.noSend

	cli.Concise = cli.Concise || env.concise
}

// rejectUnknownArgs fails the call if args carries a key the tool's schema does
// not declare (fixed-schema contract).
func rejectUnknownArgs(b *toolBinding, args map[string]any) error {
	if len(args) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, f := range b.node.Flags {
		if f.Hidden || f.Name == helpFlagName {
			continue
		}
		known[f.Name] = true
	}
	for _, p := range b.node.Positional {
		known[p.Name] = true
	}
	if b.readOnly() {
		known[conciseArg] = true // synthetic flag injected into read-tool schemas
	}
	if b.env.offersMailboxArg(b.name) {
		known[mailboxArg] = true // synthetic flag injected when --allow-mailbox is set
	}
	for k := range args {
		if !known[k] {
			return fmt.Errorf("unknown argument %q for tool %q", k, b.name)
		}
	}
	return nil
}

// callSlot admits one tool call at a time. Unlike a mutex, waiting for it
// honours the call's cancellation and deadline.
var callSlot = make(chan struct{}, 1)

func acquireCallSlot(ctx context.Context) (release func(), err error) {
	select {
	case callSlot <- struct{}{}:
		return func() { <-callSlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callTimeout is a call's whole budget, from the server's --timeout: 60
// seconds when unset, at most 600.
func callTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 60
	}
	if seconds > 600 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

// outFlagName is the flag through which a tool writes local files.
const outFlagName = "out"

// withDownloadOut takes an agent's relative out path, or the flag's default,
// under the operator's --download-dir. Kong would otherwise resolve both
// against the server's working directory. The command's saveScope still checks
// whatever results, so this only decides where a relative path points.
func withDownloadOut(b *toolBinding, args map[string]any) map[string]any {
	if !b.env.savesEnabled() {
		return args
	}
	for _, f := range b.node.Flags {
		if f.Name != outFlagName {
			continue
		}
		v, given := args[outFlagName]
		var out string
		switch {
		case given:
			s := sprintArg(v)
			if filepath.IsAbs(s) || strings.HasPrefix(s, "~") {
				return args
			}
			out = filepath.Join(b.env.saves.path, s)
		case f.HasDefault && f.Default != "":
			out = b.env.saves.path
		default:
			return args
		}
		copied := make(map[string]any, len(args)+1)
		for k, val := range args {
			copied[k] = val
		}
		copied[outFlagName] = out
		return copied
	}
	return args
}

// failedResult reports a failed call: whatever it printed, capped like any
// output, then the error, which is kept even when the output fills the cap.
func failedResult(stdout, stderr string, runErr error, limit int) *mcp.CallToolResult {
	parts := []string{}
	if s := strings.TrimSpace(stdout); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		parts = append(parts, s)
	}
	msg := capText("error: "+runErr.Error(), limit)
	if len(parts) > 0 {
		msg = capText(strings.Join(parts, "\n"), limit) + "\n" + msg
	}
	return errorResult(msg)
}

// capText truncates s to at most limit bytes (on a UTF-8 boundary), appending a
// notice so the agent knows output was clipped rather than silently complete.
func capText(s string, limit int) string {
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[output truncated at %d bytes; refine the query or raise --max-output-bytes]", limit)
}

// errorResult builds an IsError tool result as a {code, message, action} JSON
// envelope so an agent gets a machine classification plus an actionable next
// step, not just a raw Graph error.
func errorResult(msg string) *mcp.CallToolResult {
	code, action := classifyError(msg)
	env := map[string]string{"code": code, "message": msg}
	if action != "" {
		env["action"] = action
	}
	body, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		body = []byte(msg)
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
}

// The stable codes an agent may branch on. They are part of the tool contract,
// so they are named once here rather than repeated at each return.
const (
	codeUnauthenticated = "unauthenticated"
	codeForbidden       = "forbidden"
	codeNotFound        = "not_found"
	codeRateLimited     = "rate_limited"
	codeInvalidInput    = "invalid_input"
	codeUnclassified    = "error"
)

// missingScopeAction is the advice for a failure a fresh login can fix, as
// distinct from an Exchange delegation, which it cannot.
const missingScopeAction = "the signed-in token may lack a required scope; " +
	"re-run `olk auth login --scope <Scope>` (add --enterprise for work/school-only scopes)"

// classifyError maps a free-text error into a stable code and a recovery action.
// Codes mirror common Graph failure modes (auth/scope/not-found/throttling) so
// agents can branch on `code` instead of pattern-matching prose.
func classifyError(msg string) (code, action string) {
	// Every failed delegated send carries the grant hint, which names the scope,
	// so the hint alone says nothing about why the call failed — a stale ID and a
	// throttle arrive wearing it too. Pair it with an actual refusal before
	// reading it as one.
	// The paired signals are all denials. A bare 403 is not among them: Graph also
	// uses it for licensing and conditional access, and pairing on it would give
	// every such failure the three-grant diagnosis.
	refusedSend := strings.Contains(msg, "ErrorSendAsDenied") ||
		(strings.Contains(msg, "Mail.Send.Shared") &&
			(strings.Contains(msg, "Access is denied") ||
				strings.Contains(msg, "ErrorAccessDenied") ||
				strings.Contains(msg, "Forbidden")))

	switch {
	case strings.Contains(msg, "no account configured"),
		strings.Contains(msg, "no account"),
		strings.Contains(msg, "InvalidAuthenticationToken"),
		strings.Contains(msg, "401"):
		return codeUnauthenticated, "run `olk auth login` first (or re-run it if the token expired)"
	// A scope Graph names outright is fixable by signing in again, so it is
	// matched before the delegated-send case below, which mentions scope names
	// in its own advice and would otherwise swallow it.
	case strings.Contains(msg, "InsufficientScope"),
		strings.Contains(msg, "Read.Shared"):
		return codeForbidden, missingScopeAction
	// Sending as another mailbox usually fails on an Exchange delegation rather
	// than a scope, and no re-login supplies one. Graph sometimes says so with
	// ErrorSendAsDenied and sometimes only with a bare "Access is denied", which
	// no other case here matches.
	case refusedSend:
		return codeForbidden, "a refused send as another mailbox is usually a missing grant, of which there are three: the Mail.Send.Shared scope, Send As or Send on Behalf Of on that mailbox in Exchange, and Full Access on it. Only the first comes from signing in again; the other two are administrator-managed in Exchange"
	case strings.Contains(msg, "ErrorAccessDenied"),
		strings.Contains(msg, "Forbidden"),
		strings.Contains(msg, "403"):
		return codeForbidden, missingScopeAction
	case strings.Contains(msg, "ErrorItemNotFound"),
		strings.Contains(msg, "ResourceNotFound"),
		strings.Contains(msg, "404"):
		return codeNotFound, "the target mailbox or item was not found; verify Full Access and that the ID was listed from the target mailbox before retrying"
	case strings.Contains(msg, "TooManyRequests"),
		strings.Contains(msg, "throttl"),
		strings.Contains(msg, "429"):
		return codeRateLimited, "back off and retry after a short delay"
	case strings.Contains(msg, "unknown argument"),
		strings.Contains(msg, "invalid arguments"),
		strings.Contains(msg, "parse error"),
		strings.Contains(msg, "missing required argument"):
		return codeInvalidInput, "check the tool's input schema and retry with valid arguments"
	}
	return codeUnclassified, ""
}

// buildArgv reconstructs a CLI argv from a tool's arguments. Ordering is
// [path..., --json, flags..., --, positionals...]: --json and all option flags
// precede the literal "--" so kong never treats them as positionals.
func buildArgv(b *toolBinding, args map[string]any) ([]string, error) {
	argv := append([]string{}, b.path...)
	args = withDownloadOut(b, args)

	for _, f := range b.node.Flags {
		if f.Hidden || f.Name == helpFlagName {
			continue
		}
		v, ok := args[f.Name]
		if !ok {
			continue
		}
		toks, err := flagTokens(f, v)
		if err != nil {
			return nil, err
		}
		argv = append(argv, toks...)
	}

	// Honor the synthetic concise flag injected into read-tool schemas: it maps
	// to olk's global --concise rather than a per-command flag.
	if b.readOnly() {
		if on, _ := asBool(args[conciseArg]); on {
			argv = append(argv, "--concise")
		}
	}

	// Destructive tools require --force at the CLI to confirm; under MCP, naming
	// the tool via --allow-destructive is itself the deliberate confirmation, so
	// supply --force here (the --no-write capability guard still vetoes at the API
	// layer regardless).
	if b.tier == tierDestructive {
		argv = append(argv, "--force")
	}

	// Resolve the mailbox in one place so precedence is unambiguous: a permitted
	// per-call choice wins, otherwise the mailbox the server was started with.
	// The schema advertises the permitted values, but the schema is only advice
	// to the model — this check is what actually refuses anything else.
	mailbox := b.env.mailbox
	if mailbox != "" && mailboxScopedButUnaware(b.name) {
		// exposes() already withholds these, so reaching here means a tool was
		// registered in error. Refuse rather than run it against the wrong
		// mailbox.
		return nil, fmt.Errorf("tool %q cannot target mailbox %q; it always acts on the signed-in user's own mailbox",
			b.name, mailbox)
	}
	if raw, ok := args[mailboxArg]; ok {
		chosen := strings.TrimSpace(sprintArg(raw))
		if !b.env.offersMailboxArg(b.name) {
			return nil, fmt.Errorf("tool %q does not accept a %s argument", b.name, mailboxArg)
		}
		if !b.env.mailboxAllowed(chosen) {
			return nil, fmt.Errorf("mailbox %q is not permitted; this server allows %s",
				chosen, strings.Join(b.env.allowMailbox, ", "))
		}
		mailbox = chosen
	}
	// Appended even when empty. Kong re-reads OLK_MAILBOX on every parse, so
	// omitting the flag is not the same as clearing it: a server launched with no
	// mailbox would otherwise inherit whatever the operator's shell happened to
	// export.
	//
	// --json forces structured output whatever the command would default to.
	argv = append(argv, "--mailbox", mailbox, "--json")

	pos := make([]string, 0, len(b.node.Positional))
	for _, p := range b.node.Positional {
		v, ok := args[p.Name]
		if !ok {
			if p.Required {
				return nil, fmt.Errorf("missing required argument %q", p.Name)
			}
			continue
		}
		pos = append(pos, sprintArg(v))
	}
	if len(pos) > 0 {
		argv = append(argv, "--")
		argv = append(argv, pos...)
	}

	return argv, nil
}

// Every value is joined to its flag as --name=value, so a value that begins
// with "--" stays a value. A plain boolean is a switch, so false leaves it
// off, as omitting it would; a pointer boolean is a setting the command reads
// only when given, so an explicit false is passed on: `"all-day": false` must
// reach calendar update.
func flagTokens(f *kong.Flag, v any) ([]string, error) {
	name := "--" + f.Name + "="
	switch {
	case f.IsBool():
		on, err := asBool(v)
		if err != nil {
			return nil, fmt.Errorf("flag %q: %w", f.Name, err)
		}
		if !on && f.Target.Kind() != reflect.Pointer {
			return nil, nil
		}
		return []string{name + strconv.FormatBool(on)}, nil
	case f.IsSlice():
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("flag %q expects an array", f.Name)
		}
		toks := make([]string, 0, len(arr))
		for _, e := range arr {
			toks = append(toks, name+sprintArg(e))
		}
		return toks, nil
	default:
		return []string{name + sprintArg(v)}, nil
	}
}

func asBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		return b == "true", nil
	default:
		return false, fmt.Errorf("expects a boolean")
	}
}

// sprintArg renders a JSON-decoded argument value as a CLI token. JSON numbers
// decode to float64; integral values are emitted without a decimal point or
// exponent so flags like --top receive "25", not "25" via "2.5e+01".
func sprintArg(v any) string {
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(n)
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}
