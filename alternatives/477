package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestHubIntegrityOnlyAcceptsNonHubSnapshots(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, schema string
		corrupt      bool
	}{
		{"registry", "CREATE TABLE tenants (id TEXT PRIMARY KEY)", false},
		{"auth", "CREATE TABLE sessions (id TEXT PRIMARY KEY)", false},
		{"corrupt", "CREATE TABLE records (id TEXT PRIMARY KEY)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "restored.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if _, err := db.ExecContext(t.Context(), test.schema); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if test.corrupt {
				if err := os.WriteFile(path, []byte("corrupted restore"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := newHubCommandWithRun("test", func(string) string { return "" }, nil)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"verify", "--integrity-only", "--database", path})
			err = cmd.ExecuteContext(t.Context())
			if (err != nil) != test.corrupt {
				t.Fatalf("verify error = %v, output = %s", err, &output)
			}
			if !test.corrupt {
				var result struct{ Tables int }
				if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Tables != 1 {
					t.Fatalf("verify output = %s, error = %v", &output, err)
				}
			}
		})
	}
}

func TestHubRecoveryCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: path, GitHubDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	restored := filepath.Join(t.TempDir(), "restored.db")
	const token = "example-fresh-administrator-token-for-recovery"
	for _, test := range []struct {
		name string
		args []string
		fail bool
	}{
		{"missing source", []string{"verify"}, true},
		{"missing output", []string{"backup", "--database", path}, true},
		{"verify", []string{"verify", "--database", path}, false},
		{"backup", []string{"backup", "--database", path, "--output", backup}, false},
		{"existing backup", []string{"backup", "--database", path, "--output", backup}, true},
		{"invalid environment", []string{"restore", "--database", backup, "--output", restored, "--admin-token-env", "BAD=ENV"}, true},
		{"restore", []string{"restore", "--database", backup, "--output", restored}, false},
		{"verify restore", []string{"verify", "--database", restored}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := newHubCommandWithRun("test", func(string) string { return token }, nil)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(test.args)
			err := cmd.ExecuteContext(t.Context())
			if (err != nil) != test.fail {
				t.Fatalf("error = %v, output = %s", err, &output)
			}
			if strings.Contains(output.String(), token) {
				t.Fatal("command exposed administrator token")
			}
			if !test.fail && test.args[0] != "backup" {
				var result hubserver.RecoveryResult
				if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.SchemaVersion == 0 {
					t.Fatalf("invalid recovery output: %s, %v", &output, err)
				}
			}
		})
	}
	service, err = hubserver.Open(t.Context(), hubserver.Config{DatabasePath: restored, GitHubDisabled: true, InitialAdminToken: []byte(token)})
	if err != nil {
		t.Fatalf("start restored Hub without an original bootstrap principal: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}
