package cmd

// This file is owned by the mailmux fork, not upstream olk. Upstream code
// reaches it only through runSelected and withExtraOptions, so plain olk, which
// registers nothing here, behaves exactly as before.

import "github.com/alecthomas/kong"

// Router decides whether a parsed command runs through mailmux's own handlers
// instead of the upstream command's Run method.
type Router interface {
	// Route returns handled=true when it served the command, in which case err
	// is the command's result and the upstream Run method is not called. Any
	// non-nil err stops the command, whatever handled says: a router that
	// failed must never fall through to an unguarded upstream command.
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
		handled, err := router.Route(kctx, rc)
		if err != nil || handled {
			return err
		}
	}
	return kctx.Run(rc)
}

// programName names the program in version output and as the MCP server's
// implementation name. Help text gets it from the kong.Name option instead.
var programName = "olk"

// Branding renames the program for a build other than olk.
type Branding struct {
	Name        string
	Description string
}

// SetBranding renames the program in help, version output and the MCP
// server's name. kong applies Name after building the model, so the
// registered kong.Name overrides upstream's.
func SetBranding(b Branding) {
	programName = b.Name
	RegisterKongOptions(kong.Name(b.Name), kong.Description(b.Description))
}
