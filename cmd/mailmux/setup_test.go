package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rlrghb/olkcli/internal/config"
)

// fakeEnv runs isolateEnv against an in-memory environment and returns it.
func fakeEnv(t *testing.T, environ ...string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, kv := range environ {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	setenv := func(k, v string) error { env[k] = v; return nil }
	unsetenv := func(k string) error { delete(env, k); return nil }
	if err := isolateEnv(environ, setenv, unsetenv); err != nil {
		t.Fatalf("isolateEnv: %v", err)
	}
	return env
}

func TestIsolateEnv_DropsOlkVariables(t *testing.T) {
	got := fakeEnv(t, "OLK_ACCOUNT=a", "OLK_CONFIG_DIR=/olk", "HOME=/h")
	want := map[string]string{"HOME": "/h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestIsolateEnv_MapsMailmuxVariables(t *testing.T) {
	got := fakeEnv(t, "MAILMUX_ACCOUNT=b", "MAILMUX_NO_SEND=1")
	want := map[string]string{
		"MAILMUX_ACCOUNT": "b", "MAILMUX_NO_SEND": "1",
		"OLK_ACCOUNT": "b", "OLK_NO_SEND": "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestIsolateEnv_MailmuxWinsInEitherOrder(t *testing.T) {
	for _, environ := range [][]string{
		{"OLK_ACCOUNT=a", "MAILMUX_ACCOUNT=b"},
		{"MAILMUX_ACCOUNT=b", "OLK_ACCOUNT=a"},
	} {
		if got := fakeEnv(t, environ...)["OLK_ACCOUNT"]; got != "b" {
			t.Errorf("%v: OLK_ACCOUNT = %q, want b", environ, got)
		}
	}
}

func TestIsolateEnv_KeepsEqualsInValues(t *testing.T) {
	if got := fakeEnv(t, "MAILMUX_SELECT=a=b")["OLK_SELECT"]; got != "a=b" {
		t.Errorf("OLK_SELECT = %q, want a=b", got)
	}
}

func TestIsolateEnv_IgnoresBarePrefix(t *testing.T) {
	got := fakeEnv(t, "MAILMUX_=x")
	if _, ok := got["OLK_"]; ok {
		t.Errorf("env = %v; a bare MAILMUX_ must not set OLK_", got)
	}
}

// restoreConfig puts the config globals back when the test ends.
func restoreConfig(t *testing.T) {
	t.Helper()
	ns, id, cid := config.Namespace, config.DefaultClientID, clientID
	t.Cleanup(func() {
		config.Namespace, config.DefaultClientID, clientID = ns, id, cid
	})
}

func TestConfigure_DefaultNamespaceIsMailmux(t *testing.T) {
	restoreConfig(t)
	t.Setenv("OLK_CONFIG_DIR", "")
	before := config.DefaultClientID
	configure()
	if config.Namespace != "mailmux" {
		t.Errorf("Namespace = %q, want mailmux", config.Namespace)
	}
	if base := filepath.Base(config.ConfigDir()); base != "mailmux" {
		t.Errorf("ConfigDir() = %q, want a directory named mailmux", config.ConfigDir())
	}
	if config.DefaultClientID != before {
		t.Errorf("DefaultClientID changed to %q with no clientID set", config.DefaultClientID)
	}
}

func TestConfigure_ClientIDOverride(t *testing.T) {
	restoreConfig(t)
	clientID = "11111111-2222-3333-4444-555555555555"
	configure()
	if config.DefaultClientID != clientID {
		t.Errorf("DefaultClientID = %q, want %q", config.DefaultClientID, clientID)
	}
}
