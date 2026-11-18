package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestHostCacheWritableRoots(t *testing.T) {
	paths, err := toolcache.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	build, modules := paths.Build, paths.Modules
	for _, tt := range []struct {
		name, policy         string
		restricted, writable bool
	}{
		{"workspace write", "workspace-write", false, true},
		{"full access", "danger-full-access", false, false},
		{"read only", "read-only", false, false},
		{"restricted worker", "workspace-write", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hostCacheWritableRoots(context.Background(), Options{ThreadSandbox: tt.policy}, []string{"/existing"}, tt.restricted)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"/existing"}
			if tt.writable {
				want = append(want, build, modules)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("roots=%v want=%v", got, want)
			}
		})
	}
}

func TestHostCacheDiscoveryAcrossTurns(t *testing.T) {
	if os.Getenv("DETENT_TEST_CACHE_TURNS") != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHostCacheDiscoveryAcrossTurns$")
		cmd.Env = append(os.Environ(), "DETENT_TEST_CACHE_TURNS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	root := t.TempDir()
	t.Setenv("PATH", root) // Go is absent; provider turns must still start.
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	var first []string
	for turn := range 3 {
		transport := newFakeAppServerTransport([]Message{
			responseMessage(t, 1, `{"userAgent":"codex-cli/test"}`),
			responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
			responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
			notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
		})
		server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		backend, err := NewAgentBackend(server, Options{ThreadSandbox: "workspace-write"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: root, Prompt: "test", Model: "gpt-5-codex"}, nil)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		roots, err := hostCacheWritableRoots(t.Context(), Options{ThreadSandbox: "workspace-write"}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if turn == 0 {
			first = roots
		} else if !reflect.DeepEqual(roots, first) {
			t.Fatalf("discovery repeated: %v != %v", roots, first)
		}
		if len(transport.sentMessages()) != 4 {
			t.Fatal("provider was not contacted")
		}
		t.Setenv("GOCACHE", filepath.Join(root, "changed"))
		t.Setenv("GOMODCACHE", filepath.Join(root, "changed-mod"))
	}
}
