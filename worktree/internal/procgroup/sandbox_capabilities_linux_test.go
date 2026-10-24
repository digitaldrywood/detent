package procgroup

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
)

type sandboxCapabilityEvidence struct {
	Capabilities map[string]string
	Directory    string
	Value        string
	Argument     string
	OwnGroup     bool
}

func sandboxCapabilityState(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (key == "CapInh" || key == "CapPrm" || key == "CapEff" || key == "CapAmb") {
			state[key] = strings.TrimSpace(value)
		}
	}
	return state
}

func TestSandboxCapabilityChild(t *testing.T) {
	if os.Getenv("DETENT_CAPABILITY_TEST_CHILD") != "1" {
		return
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(sandboxCapabilityEvidence{
		Capabilities: sandboxCapabilityState(t), Directory: directory,
		Value: os.Getenv("DETENT_CAPABILITY_TEST_VALUE"), Argument: os.Args[len(os.Args)-1],
		OwnGroup: group == os.Getpid(),
	}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestConfigureSandboxCapabilities(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	parent := sandboxCapabilityState(t)
	var ordinary map[string]string
	for _, tier := range []string{"", isolation.NativeTrusted, isolation.Sandbox} {
		t.Run("tier_"+tier, func(t *testing.T) {
			ctx := t.Context()
			if tier != "" {
				ctx = isolation.WithPolicy(ctx, isolation.Policy{Tier: tier})
			}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSandboxCapabilityChild$", "--", "literal argument")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "DETENT_CAPABILITY_TEST_CHILD=1", "DETENT_CAPABILITY_TEST_VALUE=kept")
			originalPath, originalArgs := cmd.Path, strings.Join(cmd.Args, "\x00")
			Configure(ctx, cmd)
			Configure(ctx, cmd)
			if tier != isolation.Sandbox && (cmd.Path != originalPath || strings.Join(cmd.Args, "\x00") != originalArgs) {
				t.Fatal("ordinary launch program or arguments changed")
			}
			raw, err := cmd.Output()
			if err != nil {
				t.Fatalf("child launch: %v", err)
			}
			var evidence sandboxCapabilityEvidence
			if err := json.Unmarshal(raw, &evidence); err != nil {
				t.Fatalf("child evidence: %v", err)
			}
			if evidence.Directory != cmd.Dir || evidence.Value != "kept" || evidence.Argument != "literal argument" || !evidence.OwnGroup {
				t.Fatalf("launch metadata changed: %+v", evidence)
			}
			if tier == "" {
				ordinary = evidence.Capabilities
			}
			for key, value := range evidence.Capabilities {
				want := ordinary[key]
				if tier == isolation.Sandbox && (key == "CapInh" || key == "CapAmb" || os.Geteuid() != 0) {
					want = "0000000000000000"
				}
				if value != want {
					t.Fatalf("%s = %s, want %s", key, value, want)
				}
			}
			for key, value := range sandboxCapabilityState(t) {
				if value != parent[key] {
					t.Fatalf("parent %s changed", key)
				}
			}
		})
	}

	t.Run("missing_drop_support", func(t *testing.T) {
		ctx := isolation.WithPolicy(t.Context(), isolation.Policy{Tier: isolation.Sandbox})
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSandboxCapabilityChild$")
		cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: []uintptr{0}}
		t.Setenv("PATH", t.TempDir())
		Configure(ctx, cmd)
		if !errors.Is(cmd.Run(), exec.ErrNotFound) {
			t.Fatal("required missing drop support did not fail before launch")
		}
		if cmd.Process != nil || len(cmd.SysProcAttr.AmbientCaps) != 0 {
			t.Fatal("failed child launched or retained requested capabilities")
		}
	})

	t.Run("sandbox_without_inheritance", func(t *testing.T) {
		inherited, err := strconv.ParseUint(parent["CapInh"], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		if inherited != 0 {
			t.Skip("parent has inherited capabilities; covered by real drop fixture")
		}
		ctx := isolation.WithPolicy(t.Context(), isolation.Policy{Tier: isolation.Sandbox})
		cmd := exec.CommandContext(ctx, filepath.Join(t.TempDir(), "does-not-run"))
		originalPath := cmd.Path
		t.Setenv("PATH", t.TempDir())
		Configure(ctx, cmd)
		if cmd.Err != nil || cmd.Path != originalPath {
			t.Fatal("zero-capability launch unnecessarily requires drop utility")
		}
	})
}
