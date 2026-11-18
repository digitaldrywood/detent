package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTenantImportRequiresExplicitPostgresArchiveConfiguration(t *testing.T) {
	valid := "storage:\n  backend: postgres\ntenant_postgres:\n  host: example.invalid\n  port: 5432\n  database: example\n  user: example_runtime\n  password_env: EXAMPLE_POSTGRES_PASSWORD\n  sslmode: verify-full\n  max_connections: 2\n  serving_connections: 1\n  background_connections: 1\nattachments:\n  bucket: example\n  region: example\n"
	for _, test := range []struct {
		name            string
		config          string
		missingPassword bool
		wantError       bool
	}{
		{name: "explicit offline configuration", config: valid},
		{name: "missing password", config: valid, missingPassword: true, wantError: true},
		{name: "wrong backend", config: strings.Replace(valid, "backend: postgres", "backend: sqlite", 1), wantError: true},
		{name: "missing archive", config: strings.Split(valid, "attachments:")[0], wantError: true},
		{name: "multiple documents", config: valid + "---\nstorage: {}\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "entry.yaml")
			if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			config, archive, err := readTenantImportConfig(path, func(name string) string {
				switch name {
				case "EXAMPLE_POSTGRES_PASSWORD":
					if test.missingPassword {
						return ""
					}
					return "example-secret"
				case "DETENT_ATTACHMENTS_ACCESS_KEY_ID":
					return "example-key"
				case "DETENT_ATTACHMENTS_SECRET_ACCESS_KEY":
					return "example-archive-secret"
				default:
					t.Fatalf("offline import read unrelated credential %q", name)
					return ""
				}
			})
			if (err != nil) != test.wantError {
				t.Fatalf("configuration error=%v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "example-secret") || strings.Contains(err.Error(), "example-archive-secret") {
					t.Fatal("configuration error leaked credentials")
				}
				return
			}
			if config.Connection.Host != "example.invalid" || config.Connection.User != "example_runtime" || config.Connection.Password != "example-secret" || archive.AccessKeyID != "example-key" || archive.SecretAccessKey != "example-archive-secret" {
				t.Fatal("offline import did not use the explicit target and archive credentials")
			}
		})
	}
}
