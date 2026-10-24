package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceBinaryPreflightsBeforeReplacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		preflightErr  error
		wantTarget    string
		wantBackup    bool
		wantBeforeRun bool
	}{
		{
			name:         "rejected candidate leaves current binary untouched",
			preflightErr: errors.New("candidate cannot boot configuration"),
			wantTarget:   "old",
		},
		{
			name:          "accepted candidate preserves rollback binary",
			wantTarget:    "new",
			wantBackup:    true,
			wantBeforeRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			target := filepath.Join(dir, "detent")
			backup := PreviousBinaryPath(target)
			if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
				t.Fatalf("WriteFile(target) error = %v", err)
			}
			beforeRun := false
			err := ReplaceBinary(context.Background(), Replacement{
				Target:         target,
				Binary:         []byte("new"),
				Mode:           0o755,
				GOOS:           "linux",
				BackupPath:     backup,
				PreserveBackup: true,
				Preflight: func(_ context.Context, path string) error {
					if path == target {
						t.Fatal("Preflight() received installed target, want staged candidate")
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if string(raw) != "new" {
						return errors.New("unexpected candidate content")
					}
					return tt.preflightErr
				},
				BeforeReplace: func(_ context.Context, gotTarget string, gotBackup string) error {
					beforeRun = true
					if gotTarget != target || gotBackup != backup {
						t.Fatalf("BeforeReplace(%q, %q), want (%q, %q)", gotTarget, gotBackup, target, backup)
					}
					return nil
				},
				Verify: func(_ context.Context, path string) (string, error) {
					raw, err := os.ReadFile(path)
					if err != nil {
						return "", err
					}
					if string(raw) != "new" {
						return "", errors.New("installed content is not candidate")
					}
					return "version: v1.2.4", nil
				},
			})
			if tt.preflightErr == nil && err != nil {
				t.Fatalf("ReplaceBinary() error = %v", err)
			}
			if tt.preflightErr != nil && (err == nil || !strings.Contains(err.Error(), tt.preflightErr.Error())) {
				t.Fatalf("ReplaceBinary() error = %v, want containing %q", err, tt.preflightErr)
			}

			raw, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("ReadFile(target) error = %v", err)
			}
			if string(raw) != tt.wantTarget {
				t.Fatalf("target = %q, want %q", raw, tt.wantTarget)
			}
			if beforeRun != tt.wantBeforeRun {
				t.Fatalf("BeforeReplace() ran = %t, want %t", beforeRun, tt.wantBeforeRun)
			}
			backupRaw, backupErr := os.ReadFile(backup)
			if tt.wantBackup {
				if backupErr != nil {
					t.Fatalf("ReadFile(backup) error = %v", backupErr)
				}
				if string(backupRaw) != "old" {
					t.Fatalf("backup = %q, want old", backupRaw)
				}
			} else if !errors.Is(backupErr, os.ErrNotExist) {
				t.Fatalf("ReadFile(backup) error = %v, want not exist", backupErr)
			}
		})
	}
}
