package cmd

// This file is owned by the mailmux fork, not upstream olk. Upstream code
// reaches it only through runSelected and withExtraOptions, so plain olk, which
// registers nothing here, behaves exactly as before.

import "github.com/alecthomas/kong"

// Router decides whether a parsed command runs through mailmux's own handlers
// instead of the upstream command's Run method.
type Router interface {
	// Route returns handled=true when it served the command, in which case err
	// is the command's result and the upstream Run method is not called.
	Route(kctx *kong.Context, rc *RunContext) (handled bool, err error)
}

var (
	router           Router
	extraKongOptions []kong.Option
)

// SetRouter registers the router every command (CLI and MCP) is offered
// first. nil restores the default of always running the upstream command.
func SetRouter(r Router) { router = r }

// RegisterKongOptions adds options applied after upstream's own on every
// parser, CLI and MCP alike: extra commands and flags (kong.Embed), branding.
func RegisterKongOptions(opts ...kong.Option) {
	extraKongOptions = append(extraKongOptions, opts...)
}

// withExtraOptions returns base followed by the registered options.
func withExtraOptions(base ...kong.Option) []kong.Option {
	return append(base, extraKongOptions...)
}

// runSelected runs a parsed command: through the router when one is set and
// takes it, otherwise through the command's own Run method.
func runSelected(kctx *kong.Context, rc *RunContext) error {
	if router != nil {
		if handled, err := router.Route(kctx, rc); handled {
			return err
		}
	}
	return kctx.Run(rc)
}
