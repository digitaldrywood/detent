package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestPostgresMigrationUsesOnlyExplicitConnection(t *testing.T) {
	for _, test := range []struct {
		name string
		omit string
		fail bool
	}{
		{name: "explicit connection"},
		{name: "missing host", omit: "--host"},
		{name: "missing port", omit: "--port"},
		{name: "missing database", omit: "--database"},
		{name: "missing login", omit: "--user"},
		{name: "missing password reference", omit: "--password-env"},
		{name: "missing TLS mode", omit: "--sslmode"},
		{name: "migration failure", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			called := false
			migrationErr := errors.New("migration rejected")
			cmd := newHubPostgresMigrationCommand(func(name string) string {
				if name != "EXAMPLE_MIGRATION_PASSWORD" {
					t.Fatalf("unexpected environment lookup %q", name)
				}
				return "example-secret"
			}, func(ctx context.Context, connection pq.Config) error {
				called = true
				if ctx != t.Context() || connection.Host != "example.invalid" || connection.Port != 5432 || connection.Database != "example" || connection.User != "example_migration" || connection.Password != "example-secret" || connection.SSLMode != "verify-full" || connection.Passfile != "" || connection.Service != "" || connection.Options != "" || len(connection.Runtime) != 0 {
					t.Fatal("migration did not receive the explicit connection and caller context")
				}
				if test.fail {
					return migrationErr
				}
				return nil
			})
			args := []string{"--host", "example.invalid", "--port", "5432", "--database", "example", "--user", "example_migration", "--password-env", "EXAMPLE_MIGRATION_PASSWORD", "--sslmode", "verify-full"}
			if test.omit != "" {
				for i := 0; i < len(args); i += 2 {
					if args[i] == test.omit {
						args = append(args[:i], args[i+2:]...)
						break
					}
				}
			}
			cmd.SetArgs(args)
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			err := cmd.ExecuteContext(t.Context())
			if called != (test.omit == "") || (err != nil) != (test.omit != "" || test.fail) || test.fail && !errors.Is(err, migrationErr) {
				t.Fatalf("called=%v error=%v", called, err)
			}
			if strings.Contains(output.String(), "example-secret") {
				t.Fatal("migration output disclosed the password")
			}
		})
	}
}
