package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReplaceBinaryVersionStart(t *testing.T) {
	candidate, err := os.ReadFile("/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name        string
		holdWriter  bool
		release     bool
		cancel      bool
		denyExec    bool
		missingFile bool
		wrongCommit bool
		maxStarts   int
		wantErr     error
		wantStarts  int
		wantWaits   int
	}{
		{name: "writer released", holdWriter: true, release: true, wantStarts: 2, wantWaits: 1},
		{name: "persistent writer", holdWriter: true, wantErr: syscall.ETXTBSY, wantStarts: 3, wantWaits: 2},
		{name: "two-start bound", holdWriter: true, maxStarts: 2, wantErr: syscall.ETXTBSY, wantStarts: 2, wantWaits: 1},
		{name: "cancel during wait", holdWriter: true, cancel: true, wantErr: context.Canceled, wantStarts: 1, wantWaits: 1},
		{name: "permission denied", denyExec: true, wantErr: syscall.EACCES, wantStarts: 1},
		{name: "candidate exits unsuccessfully", missingFile: true, wantStarts: 1},
		{name: "wrong commit", wrongCommit: true, wantStarts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dir := t.TempDir()
			target := filepath.Join(dir, "detent")
			if err := os.WriteFile(target, old, 0o755); err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			inputs := t.TempDir()
			versionFile := filepath.Join(inputs, "version")
			commit := testUpdatedCommit
			if tt.wrongCommit {
				commit = testPreviousCommit
			}
			identity := "version: v1.2.4\ncommit: " + commit + "\n"
			if !tt.missingFile {
				if err := os.WriteFile(versionFile, []byte(identity), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var writer *os.File
			closeWriter := func() {
				if writer != nil {
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					writer = nil
				}
			}
			t.Cleanup(closeWriter)
			var staged string
			var starts, waits, protocolChecks, replacements int
			verify := binaryIdentityVerifier(func(ctx context.Context, path string) (string, error) {
				return verifyBinaryVersionWith(ctx, path, func(cmd *exec.Cmd) ([]byte, error) {
					cmd.Dir = inputs
					output, err := cmd.CombinedOutput()
					if cmd.Path != target {
						starts++
						if cmd.Path != staged {
							t.Fatalf("started %q, want staged candidate %q", cmd.Path, staged)
						}
						if writer != nil && (!errors.Is(err, syscall.ETXTBSY) || cmd.Process != nil) {
							t.Fatalf("start with writer open: process %v, error %v", cmd.Process, err)
						}
					}
					return output, err
				})
			}, "1.2.4", testUpdatedCommit)
			maxStarts := candidateVersionMaxStarts
			if tt.maxStarts != 0 {
				maxStarts = tt.maxStarts
			}
			wait := func(ctx context.Context, delay time.Duration) bool {
				waits++
				if tt.release {
					closeWriter()
				}
				if tt.cancel {
					cancel()
					return waitForUpdateCheck(ctx, delay)
				}
				return true
			}
			probe := candidateIdentityPreflightWith(verify, func(_ context.Context, path string) error {
				protocolChecks++
				if path != staged {
					t.Fatalf("protocol check on %q, want %q", path, staged)
				}
				return nil
			}, maxStarts, wait)
			err = ReplaceBinary(ctx, Replacement{
				Target: target, Binary: candidate, Verify: verify,
				Preflight: func(ctx context.Context, path string) error {
					staged = path
					if tt.denyExec {
						if err := os.Chmod(path, 0o600); err != nil {
							return err
						}
					}
					if tt.holdWriter {
						var err error
						writer, err = os.OpenFile(path, os.O_WRONLY, 0)
						if err != nil {
							return err
						}
					}
					return probe(ctx, path)
				},
				BeforeReplace: func(context.Context, string, string) error {
					replacements++
					return nil
				},
			})
			var exitErr *exec.ExitError
			switch {
			case tt.missingFile:
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Errorf("replace error = %v, want candidate exit 1", err)
				}
			case tt.wrongCommit:
				if err == nil || !strings.Contains(err.Error(), "does not match tested release commit") {
					t.Errorf("replace error = %v, want commit mismatch", err)
				}
			case !errors.Is(err, tt.wantErr):
				t.Errorf("replace error = %v, want %v", err, tt.wantErr)
			}
			if starts != tt.wantStarts || waits != tt.wantWaits {
				t.Errorf("candidate starts/waits = %d/%d, want %d/%d", starts, waits, tt.wantStarts, tt.wantWaits)
			}
			wantPublished, wantOutput := 0, ""
			if tt.release {
				wantPublished, wantOutput = 1, identity
			}
			if protocolChecks != wantPublished || replacements != wantPublished {
				t.Errorf("protocol checks/replacements = %d/%d, want %d each", protocolChecks, replacements, wantPublished)
			}
			installed, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(original, installed) == tt.release {
				t.Errorf("installed executable replaced = %t, want %t", !os.SameFile(original, installed), tt.release)
			}
			output, err := exec.CommandContext(t.Context(), target, versionFile).CombinedOutput()
			if err != nil || string(output) != wantOutput {
				t.Errorf("installed binary output = %q, error = %v, want %q", output, err, wantOutput)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "detent" {
				t.Errorf("after staging cleanup: entries %v, error %v", entries, err)
			}
		})
	}
}

func TestInstalledVersionStartsOnce(t *testing.T) {
	for _, phase := range []string{"startup recovery", "post-install"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "detent")
			worker, err := os.ReadFile("/bin/true")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, worker, 0o755); err != nil {
				t.Fatal(err)
			}

			starts := 0
			installed := func(ctx context.Context, path string) (string, error) {
				if path != target {
					t.Fatalf("installed probe path = %q, want %q", path, target)
				}
				writer, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := writer.Close(); err != nil {
						t.Error(err)
					}
				}()
				return verifyBinaryVersionWith(ctx, path, func(cmd *exec.Cmd) ([]byte, error) {
					starts++
					return cmd.CombinedOutput()
				})
			}
			switch phase {
			case "startup recovery":
				recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
					StatePath: filepath.Join(dir, "recovery.json"), CurrentVersion: "1.2.3",
					CurrentCommit: testPreviousCommit, BinaryVerifier: installed,
				})
				recovery.state.PendingUpdate = &PendingUpdate{
					FromVersion: "1.2.3", ToVersion: "1.2.4", FromCommit: testPreviousCommit, ExecutablePath: target,
				}
				err = recovery.MarkHealthy(t.Context())
			case "post-install":
				err = ReplaceBinary(t.Context(), Replacement{
					Target: target, Binary: worker, Verify: binaryIdentityVerifier(installed, "1.2.4", testUpdatedCommit),
				})
			}
			if !errors.Is(err, syscall.ETXTBSY) || starts != 1 {
				t.Errorf("installed probe: %d starts, error %v, want one start and ETXTBSY", starts, err)
			}
		})
	}
}
