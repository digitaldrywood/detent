package cli

import (
	"os"
	"strings"
	"testing"
)

func TestCloudPlatformImportConfigRequiresPostgreSQLAndKeepsSecretsOutOfErrors(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	body := "storage:\n  backend: postgres\n  postgres_dsn_env: EXAMPLE_PLATFORM_DSN\n  max_connections: 4\n  serving_connections: 2\n  background_connections: 2\nassertion:\n  signing_key_env: EXAMPLE_ASSERTION_KEY\nattachments:\n  endpoint: https://storage.example.test\n  region: example\n  bucket: example-archive\n"
	environment := map[string]string{
		"EXAMPLE_PLATFORM_DSN":                 "postgres://example.invalid/platform",
		"EXAMPLE_ASSERTION_KEY":                seed,
		"DETENT_ATTACHMENTS_ACCESS_KEY_ID":     "example-access-key",
		"DETENT_ATTACHMENTS_SECRET_ACCESS_KEY": "example-secret-key",
	}
	path := t.TempDir() + "/cloud.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := readCloudPlatformImportConfig(path, func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.postgresDSN != environment["EXAMPLE_PLATFORM_DSN"] || config.pool.MaxConnections != 4 || config.attachments.AccessKeyID != environment["DETENT_ATTACHMENTS_ACCESS_KEY_ID"] || config.attachments.SecretAccessKey != environment["DETENT_ATTACHMENTS_SECRET_ACCESS_KEY"] {
		t.Fatal("platform import did not load the configured PostgreSQL and archive settings")
	}

	for _, test := range []struct {
		name string
		body string
		env  map[string]string
	}{
		{name: "SQLite target", body: strings.Replace(body, "backend: postgres", "backend: sqlite", 1), env: environment},
		{name: "missing attachment store", body: strings.Split(body, "attachments:")[0], env: environment},
		{name: "missing PostgreSQL environment variable", body: body, env: map[string]string{"EXAMPLE_ASSERTION_KEY": seed}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := t.TempDir() + "/cloud.yaml"
			if err := os.WriteFile(candidate, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readCloudPlatformImportConfig(candidate, func(name string) string { return test.env[name] })
			if err == nil {
				t.Fatal("invalid import configuration was accepted")
			}
			for _, secret := range []string{seed, "example-secret-key", "postgres://example.invalid/platform"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("configuration error disclosed a secret or connection string")
				}
			}
		})
	}
}

func TestCloudPlatformImportReadsValidatedRepositoryCatalog(t *testing.T) {
	t.Parallel()
	if _, err := readPlatformImportCatalog("../.."); err != nil {
		t.Fatal(err)
	}
}
