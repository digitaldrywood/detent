package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareLegacyCodexRollout(t *testing.T) {
	t.Parallel()
	for _, launchd := range []bool{false, true} {
		for _, directory := range []string{"sessions", "archived_sessions"} {
			t.Run(directory+map[bool]string{false: "/worker", true: "/launchd"}[launchd], func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				source := filepath.Join(home, ".codex")
				profileName := workerCodexProfileDir
				if launchd {
					profileName = launchdCodexProfileDir
				}
				profile := filepath.Join(source, profileName)
				if err := ensureLaunchdCodexProfile(profile); err != nil {
					t.Fatal(err)
				}
				legacy := writeRetentionRollout(t, filepath.Join(source, directory, "2026", "09", "01"), "thread-existing", "", time.Now())
				personal := writeRetentionRollout(t, filepath.Dir(legacy), "thread-personal", "", time.Now())
				original, err := os.ReadFile(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(source, directory), filepath.Join(profile, directory)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := prepareWorkerCodexHome(source, home, launchd); err != nil {
					t.Fatal(err)
				}
				if err := prepareLegacyCodexRollout(t.Context(), profile, "thread-existing"); err != nil {
					t.Fatal(err)
				}
				// Resumed writes must go to an independent file in sessions, even
				// when the legacy rollout was archived.
				local := filepath.Join(profile, "sessions", "2026", "09", "01", filepath.Base(legacy))
				copied, err := os.ReadFile(local)
				if err != nil || string(copied) != string(original) {
					t.Fatalf("resumable rollout = %q, %v; want legacy transcript", copied, err)
				}
				if err := os.WriteFile(local, append(copied, []byte("\nresumed turn")...), 0600); err != nil {
					t.Fatal(err)
				}
				if err := prepareLegacyCodexRollout(t.Context(), profile, "thread-existing"); err != nil {
					t.Fatal(err)
				}
				copied, err = os.ReadFile(local)
				if err != nil || string(copied) != string(original)+"\nresumed turn" {
					t.Fatalf("existing rollout overwritten: %q, %v", copied, err)
				}
				unchanged, err := os.ReadFile(legacy)
				if err != nil || string(unchanged) != string(original) {
					t.Fatalf("host changed: %q, %v", unchanged, err)
				}
				if _, err := os.Stat(personal); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(local), filepath.Base(personal))); !os.IsNotExist(err) {
					t.Fatalf("personal rollout copied: %v", err)
				}
			})
		}
	}
}

func TestPrepareLegacyCodexRolloutLookup(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing", "wrong metadata", "symlink", "nested symlink", "local archive", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			profile, _, err := prepareWorkerCodexHome(home, t.TempDir(), false)
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(home, "sessions")
			if mode == "local archive" {
				directory = filepath.Join(profile, "archived_sessions")
			}
			legacy := writeRetentionRollout(t, directory, "thread-existing", "", time.Now())
			original, err := os.ReadFile(legacy)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				if err := os.Remove(legacy); err != nil {
					t.Fatal(err)
				}
			case "wrong metadata":
				other := writeRetentionRollout(t, directory, "thread-personal", "", time.Now())
				if err := os.Remove(legacy); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, legacy); err != nil {
					t.Fatal(err)
				}
			case "symlink", "nested symlink":
				external := writeRetentionRollout(t, t.TempDir(), "thread-existing", "", time.Now())
				if err := os.Remove(legacy); err != nil {
					t.Fatal(err)
				}
				link, target := legacy, external
				if mode == "nested symlink" {
					link, target = filepath.Join(directory, "linked"), filepath.Dir(external)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if mode == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err = prepareLegacyCodexRollout(ctx, profile, "thread-existing")
			if mode == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled preparation = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			local := filepath.Join(profile, "sessions", filepath.Base(legacy))
			copied, err := os.ReadFile(local)
			if mode == "local archive" {
				if err != nil || string(copied) != string(original) {
					t.Fatalf("archived rollout unavailable = %q, %v", copied, err)
				}
				unchanged, err := os.ReadFile(legacy)
				if err != nil || string(unchanged) != string(original) {
					t.Fatalf("archive changed = %q, %v", unchanged, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected copied rollout = %q, %v", copied, err)
			}
		})
	}
}
