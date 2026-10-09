package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestProbeBackendTiers(t *testing.T) {
	for _, test := range []struct {
		name    string
		native  bool
		sandbox bool
		want    []string
	}{
		{"both", true, true, []string{"native-trusted", "sandbox"}},
		{"native only", true, false, []string{"native-trusted"}},
		{"unavailable", false, false, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reasons := probeBackendTiers(t.Context(), workflowconfig.AgentBackend{}, isolation.Policy{}, func(_ context.Context, _ workflowconfig.AgentBackend, policy isolation.Policy) error {
				if policy.Tier == isolation.NativeTrusted && test.native || policy.Tier == isolation.Sandbox && test.sandbox {
					return nil
				}
				return errors.New("backend sandbox tooling is unavailable")
			})
			if !test.sandbox && reasons[isolation.Sandbox] != "backend sandbox tooling is unavailable" {
				t.Fatalf("sandbox reason lost: %#v", reasons)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tiers = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBackendVersionAtLeast(t *testing.T) {
	for _, test := range []struct {
		output string
		want   bool
	}{
		{"codex-cli 0.159.2", true}, {"codex-cli 0.160.0", true},
		{"codex-cli 0.159.1", false}, {"unknown", false}, {"codex-cli 1.0.0", true},
	} {
		t.Run(test.output, func(t *testing.T) {
			if got := backendVersionAtLeast([]byte(test.output), [3]int{0, 159, 2}); got != test.want {
				t.Fatalf("version supported = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProbeBackendRejectsUnsupportedCommand(t *testing.T) {
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server --dangerously-bypass-approvals-and-sandbox"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); err == nil {
		t.Fatal("unsafe command advertised sandbox")
	}
}

func TestBackendProbeReapsDescendantPipes(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess cancellation integration")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	cmd := backendProbeCommand(ctx, "sh -c 'sleep 30 & wait'", "sh", nil)
	_, err := runBackendProbe(cmd)
	if err == nil {
		t.Fatal("probe ignored cancellation")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("probe pipe cleanup took %v", elapsed)
	}
}

func TestBackendProbeReapsChildrenAfterParentExits(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("requires Unix process groups")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := backendProbeCommand(ctx, "sh -c 'sleep 30 & echo $!'", "sh", nil)
	output, err := runBackendProbe(cmd)
	if err == nil {
		t.Fatal("probe did not report inherited output pipes")
	}
	pid := strings.TrimSpace(string(output))
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("child PID = %q: %v", pid, err)
	}
	status, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", pid).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("inspect child %s: %v", pid, err)
		}
		return
	}
	if state := strings.TrimSpace(string(status)); state != "" && !strings.HasPrefix(state, "Z") {
		t.Fatalf("probe left child %s running with state %s", pid, state)
	}
}

func TestProbeBackendCodexEnforcesSandbox(t *testing.T) {
	if os.Getenv("DETENT_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("requires an installed Codex sandbox backend")
	}
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); err != nil {
		t.Fatalf("real Codex filesystem and network enforcement probe: %v", err)
	}
}
