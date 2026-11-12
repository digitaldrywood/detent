//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodexTransportLaunchRequiresPreparedProfile(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		change  func(t *testing.T, home string, source string)
		wantErr string
	}{
		{name: "newly added user skill is disabled before launch", change: func(t *testing.T, home string, _ string) {
			writeUserSkillFixture(t, filepath.Join(home, ".agents", "skills", "late"))
		}},
		{name: "malformed source config refuses launch", wantErr: "parse Codex config", change: func(t *testing.T, _ string, source string) {
			if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = [unterminated"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unreadable new user skill refuses launch", wantErr: "user skill directory", change: func(t *testing.T, home string, _ string) {
			if os.Geteuid() == 0 {
				t.Fatal("unreadable directory fixture requires an unprivileged test process")
			}
			locked := filepath.Join(home, ".agents", "skills", "locked")
			writeUserSkillFixture(t, locked)
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			source := filepath.Join(home, "codex-source")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5.6-codex\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeUserSkillFixture(t, filepath.Join(home, ".agents", "skills", "personal"))
			marker := filepath.Join(t.TempDir(), "launched-config.toml")
			command := "env CODEX_HOME='" + source + "' sh -c 'cp \"$CODEX_HOME/config.toml\" \"" + marker + "\"; cat >/dev/null'"
			prepare := func(command string) (codexServiceCommand, error) {
				return prepareCodexCommandForService(command, runtime.GOOS, "manual", func(string) (string, bool) { return "", false }, func() (string, error) { return home, nil })
			}
			factory, _, err := newCodexTransportFactory(command, "sh", prepare)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, home, source)
			transport, err := factory.NewTransport(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewTransport() error = %v, want %q", err, tt.wantErr)
				}
				if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("provider process started after failed preparation: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := transport.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			launched, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			for _, skill := range []string{"personal", "late"} {
				if !strings.Contains(string(launched), filepath.Join(home, ".agents", "skills", skill, "SKILL.md")) {
					t.Errorf("launched config does not disable %s:\n%s", skill, launched)
				}
			}
		})
	}
}
