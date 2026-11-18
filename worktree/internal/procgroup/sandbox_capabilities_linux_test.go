package procgroup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
			cmd.Env = append(os.Environ(), "DETENT_CAPABILITY_TEST_CHILD=1", "DETENT_CAPABILITY_TEST_VALUE=kept", "GOCOVERDIR="+t.TempDir())
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
		ctx := isolation.WithPolicy(t.Context(), isolation.Policy{Tier: isolation.Sandbox})
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "DETENT_CAPABILITY_ZERO_HELPER=1", "GOCOVERDIR="+t.TempDir(),
			"DETENT_CAPABILITY_EMPTY_PATH="+t.TempDir(), "DETENT_CAPABILITY_ZERO_PATH="+filepath.Join(t.TempDir(), "does-not-run"))
		Configure(ctx, cmd)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			input.Close()
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			input.Close()
			t.Fatal(err)
		}
		finished := false
		t.Cleanup(func() {
			input.Close()
			if !finished {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		marker := make([]byte, len("ready\n"))
		if _, err := io.ReadFull(output, marker); err != nil {
			waitErr := cmd.Wait()
			finished = true
			t.Fatalf("zero-capability helper readiness: %v; exit = %v, stderr = %s", err, waitErr, &stderr)
		}
		if string(marker) != "ready\n" {
			t.Fatalf("zero-capability helper marker = %q", marker)
		}
		if _, err := io.WriteString(input, "exit\n"); err != nil {
			t.Fatal(err)
		}
		err = cmd.Wait()
		finished = true
		if err != nil {
			t.Fatalf("zero-capability helper: %v: %s", err, &stderr)
		}
	})
}
