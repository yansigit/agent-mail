# Developing mailmux

mailmux is the multi-provider build of [olk](https://github.com/rlrghb/olkcli). This
repository keeps merging upstream olk, so the fork follows one rule above all others:
**keep changes to upstream files to the hook points listed below.** The design is in
[`docs/superpowers/specs/2026-11-06-mailmux-multi-provider-design.md`](../superpowers/specs/2026-11-06-mailmux-multi-provider-design.md).

## Where new code goes

- `cmd/mailmux/`: the mailmux entry point.
- `internal/mux/…`: everything provider-related (added from milestone M1).
- Files named `mux_*.go` / `mux_*_test.go` inside upstream packages, when the code
  needs a package's unexported helpers. New files never conflict in a merge.

## Hook points in upstream files

These are the only edits to upstream files. Update this list whenever a milestone
adds a hook.

| File | Edit |
|---|---|
| `internal/cmd/root.go` | `kong.Parse(cli, withExtraOptions(…)...)`; `runSelected(ctx, runCtx)` instead of `ctx.Run(runCtx)` |
| `internal/cmd/mcp_invoke.go` | `runSelected(kctx, runCtx)` instead of `kctx.Run(runCtx)` |
| `internal/cmd/mcp_server.go` | `kong.New(cli, withExtraOptions(…)...)` in `newKongParser`; `Name: programName` for the MCP server |
| `internal/cmd/version.go` | `programName` instead of the literal `olk` in plain version output |
| `internal/config/config.go` | `DefaultClientID` is a `var` (was a `const`) |
| `CLAUDE.md` | one pointer line to this file |

The hooks themselves live in `internal/cmd/mux_hooks.go`: `SetRouter`,
`RegisterKongOptions`, `SetBranding`. With nothing registered, `olk` behaves exactly as
upstream.

## Namespaces

| Build | Namespace (config directory and keychain service) |
|---|---|
| `make build` (olk) | `olk-dev` |
| `make install` / release olk | `olk` |
| `make -f Makefile.mailmux build` | `mailmux-dev` |
| `make -f Makefile.mailmux install`, `go install ./cmd/mailmux` | `mailmux` |

`cmd/mailmux` sets the namespace itself (`main.namespace`, default `mailmux`), so a
build without flags never reads or writes olk's credentials.

## Environment variables

mailmux reads `MAILMUX_*` variables, never olk's: before parsing, `cmd/mailmux` unsets
every inherited `OLK_*` variable and copies each `MAILMUX_<X>` to `OLK_<X>`, the name
upstream's flags read. For example `MAILMUX_ACCOUNT`, `MAILMUX_CONFIG_DIR`,
`MAILMUX_KEYRING_PASSWORD`. olk itself is unaffected.

## Building and testing

```bash
make -f Makefile.mailmux build   # ./bin/mailmux, namespace mailmux-dev
make test                        # whole suite, upstream tests included
make lint
```

Upstream's own tests must keep passing unchanged; they are the check that the hooks
did not alter olk's behaviour.

## Merging upstream

```bash
git remote add upstream https://github.com/rlrghb/olkcli.git   # once
git fetch upstream
git merge upstream/main
make test && make -f Makefile.mailmux build
```

Conflicts should appear only at the hook lines above. If upstream changes one of those
call sites, re-apply the hook in the new code.

## Known limitations

- About 19 user-facing messages from upstream still say `olk …`, for example
  "Run 'olk auth login' first". They are not rewritten yet.
- Until the mailmux Microsoft app is registered (milestone M5), mailmux builds use
  upstream's Microsoft app ID unless `MAILMUX_CLIENT_ID` is passed to the build.
