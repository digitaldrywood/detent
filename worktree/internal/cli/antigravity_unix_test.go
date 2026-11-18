//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestProbeRunnerProviderAuthAntigravity(t *testing.T) {
	script := filepath.Join(t.TempDir(), "agy-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$1\" = models ] && [ -f \"$HOME/signed-in\" ] && exit 0\necho 'Error: Please sign in to view available models.' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		signedIn bool
		home     bool
		want     bool
	}{
		{name: "signed in", signedIn: true, home: true, want: true},
		{name: "signed out", home: true},
		{name: "missing home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var options workflowconfig.BackendOptions
			if test.home {
				home := t.TempDir()
				if test.signedIn {
					if err := os.WriteFile(filepath.Join(home, "signed-in"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := yaml.Unmarshal([]byte("home: "+home), &options); err != nil {
					t.Fatal(err)
				}
			}
			backend := workflowconfig.AgentBackend{ID: "agy", Kind: workflowconfig.AgentBackendAntigravity, Command: script, Options: options}
			if got := probeRunnerProviderAuth(t.Context(), backend); got != test.want {
				t.Fatalf("probeRunnerProviderAuth = %v, want %v", got, test.want)
			}
			if !test.home {
				return
			}
			err := probeBackendSignIn(t.Context(), backend)
			if test.want != (err == nil) {
				t.Fatalf("probeBackendSignIn = %v", err)
			}
			if err != nil {
				if problem := backendReadinessProblem("example-project/agy", backend.Kind, isolation.NativeTrusted, runtime.GOOS, err); problem.Message != "not signed in" || problem.FixCommand != "agy" || !strings.Contains(problem.FixHint, "options.home") {
					t.Fatalf("problem = %+v", problem)
				}
			}
		})
	}
}
