package main

import (
	"strings"

	"github.com/rlrghb/olkcli/internal/cmd"
	"github.com/rlrghb/olkcli/internal/config"
)

// Build settings, overridable with -ldflags "-X main.namespace=… -X main.clientID=…".
var (
	// namespace names mailmux's config directory and keychain service. It
	// defaults to mailmux so a plain `go install` never touches olk's.
	namespace = "mailmux"
	// clientID is mailmux's own Microsoft app ID; empty keeps upstream's
	// default until the mailmux app is registered.
	clientID = ""
)

const (
	olkEnvPrefix     = "OLK_"
	mailmuxEnvPrefix = "MAILMUX_"
)

// isolateEnv makes mailmux read MAILMUX_* settings and never olk's. It unsets
// every inherited OLK_* variable, so a shell set up for olk cannot point
// mailmux at olk's account or config directory, then copies each MAILMUX_<X>
// to OLK_<X>, the names upstream's flags read.
func isolateEnv(environ []string, setenv func(key, value string) error, unsetenv func(key string) error) error {
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, olkEnvPrefix) {
			if err := unsetenv(key); err != nil {
				return err
			}
		}
	}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		name, ok := strings.CutPrefix(key, mailmuxEnvPrefix)
		if !ok || name == "" {
			continue
		}
		if err := setenv(olkEnvPrefix+name, value); err != nil {
			return err
		}
	}
	return nil
}

// configure applies mailmux's namespace, Microsoft app ID and branding.
func configure() {
	config.Namespace = namespace
	if clientID != "" {
		config.DefaultClientID = clientID
	}
	cmd.SetBranding(cmd.Branding{
		Name:        "mailmux",
		Description: "Mail for agents across Outlook, Gmail and IMAP",
	})
}
