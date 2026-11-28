package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

const hubServePostgresConfig = `postgres:
  host: postgres.example.test
  port: 5432
  database: example_hub
  user: example_runtime
  password_env: TEST_HUB_POSTGRES_PASSWORD
  sslmode: verify-full
  max_connections: 4
  serving_connections: 2
  background_connections: 1
`

func writeHubServeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHubServeCommandPassesConfiguration(t *testing.T) {
	t.Parallel()

	wantContext := context.WithValue(context.Background(), hubContextKey{}, "request")
	configPath := writeHubServeConfig(t, hubServePostgresConfig)
	var gotContextValue string
	var gotConfig hubserver.Config
	run := func(ctx context.Context, cfg hubserver.Config) error {
		gotContextValue, _ = ctx.Value(hubContextKey{}).(string)
		gotConfig = cfg
		return nil
	}
	cmd := newHubCommandWithRun("v-test", func(name string) string {
		if name == "DETENT_HUB_SECRET_KEY_VERSION" {
			return "1"
		}
		if name == "DETENT_HUB_SECRET_KEYS" {
			return `{"1":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))) + `"}`
		}
		if name == "TEST_HUB_ADMIN_TOKEN" {
			return "  hub-admin-token  "
		}
		if name == "TEST_HUB_POSTGRES_PASSWORD" {
			return "postgres-fixture-password"
		}
		if name == entryauthority.RequestBudgetEnvironment {
			return "53s"
		}
		if name == "TEST_HUB_WEBHOOK_SECRET" {
			return "webhook-secret"
		}
		return ""
	}, run)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"serve",
		"--hosted-config", configPath,
		"--listen", "127.0.0.1:0",
		"--tls-cert", "server.crt",
		"--tls-key", "server.key",
		"--trusted-proxy",
		"--admin-token-env", "TEST_HUB_ADMIN_TOKEN",
		"--busy-timeout", "2s",
		"--shutdown-timeout", "3s",
		"--github-webhook-secret-env", "TEST_HUB_WEBHOOK_SECRET",
		"--webhook-payload-retention", "48h",
		"--webhook-maintenance-interval", "30s",
		"--reconcile-interval", "2m",
		"--full-repair-interval", "6h",
	})
	if err := cmd.ExecuteContext(wantContext); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if gotContextValue != "request" {
		t.Fatal("runner did not receive command context")
	}
	if gotConfig.SecretKeys == nil || gotConfig.SecretKeys.Version() != 1 {
		t.Fatal("secret keys were not supplied from the environment")
	}
	if gotConfig.Hosted != nil || gotConfig.DatabaseBackend != hubserver.DatabasePostgres || gotConfig.Postgres == nil {
		t.Fatal("expected local mode with PostgreSQL configuration")
	}
	connection := gotConfig.Postgres.Connection
	if connection.Host != "postgres.example.test" || connection.Port != 5432 || connection.Database != "example_hub" || connection.User != "example_runtime" || connection.SSLMode != "verify-full" || connection.Password != "postgres-fixture-password" {
		t.Fatal("PostgreSQL connection did not match the supplied configuration")
	}
	if gotConfig.ListenAddress != "127.0.0.1:0" {
		t.Fatalf("listen address = %q, want ephemeral loopback", gotConfig.ListenAddress)
	}
	if gotConfig.TLSCertFile != "server.crt" || gotConfig.TLSKeyFile != "server.key" || !gotConfig.TrustedProxy {
		t.Fatalf("transport security = cert %q key %q trusted proxy %t", gotConfig.TLSCertFile, gotConfig.TLSKeyFile, gotConfig.TrustedProxy)
	}
	if gotConfig.BusyTimeout != 2*time.Second {
		t.Fatalf("busy timeout = %s, want 2s", gotConfig.BusyTimeout)
	}
	if gotConfig.RequestBudget != 53*time.Second {
		t.Fatalf("request budget = %s, want 53s from the entry bootstrap configuration", gotConfig.RequestBudget)
	}
	if gotConfig.ShutdownTimeout != 3*time.Second {
		t.Fatalf("shutdown timeout = %s, want 3s", gotConfig.ShutdownTimeout)
	}
	if string(gotConfig.GitHubWebhookSecret) != "webhook-secret" {
		t.Fatalf("GitHub webhook secret = %q, want resolved secret", gotConfig.GitHubWebhookSecret)
	}
	if string(gotConfig.InitialAdminToken) != "hub-admin-token" {
		t.Fatalf("Hub administrator token was not resolved from its environment variable")
	}
	if gotConfig.WebhookPayloadRetention != 48*time.Hour {
		t.Fatalf("webhook payload retention = %s, want 48h", gotConfig.WebhookPayloadRetention)
	}
	if gotConfig.WebhookMaintenanceInterval != 30*time.Second {
		t.Fatalf("webhook maintenance interval = %s, want 30s", gotConfig.WebhookMaintenanceInterval)
	}
	if gotConfig.ReconcileInterval != 2*time.Minute {
		t.Fatalf("reconcile interval = %s, want 2m", gotConfig.ReconcileInterval)
	}
	if gotConfig.FullRepairInterval != 6*time.Hour {
		t.Fatalf("full repair interval = %s, want 6h", gotConfig.FullRepairInterval)
	}
	if gotConfig.Version != "v-test" {
		t.Fatalf("version = %q, want v-test", gotConfig.Version)
	}
	t.Run("request without a deadline", func(t *testing.T) {
		var applied time.Duration
		gotConfig.Deps().RequestTimeout = func(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
			applied = budget
			return context.WithTimeout(ctx, budget)
		}
		hub := hubfixture.Open(t, gotConfig)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
		request.Header.Set("Authorization", "Bearer "+hub.AdminToken())
		response := httptest.NewRecorder()
		hub.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || applied != 53*time.Second {
			t.Fatalf("health status = %d, request budget = %s, want 200 and 53s", response.Code, applied)
		}
	})
}

func TestHubServeCommandValidatesDatabase(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		body  string
		flags []string
		want  string
	}{
		{name: "missing configuration", want: "PostgreSQL Hub configuration is required"},
		{name: "obsolete database path", body: hubServePostgresConfig, flags: []string{"--database", "hub.db"}, want: "unknown flag: --database"},
		{name: "invalid PostgreSQL field", body: hubServePostgresConfig + "  unexpected: true\n", want: "hub PostgreSQL configuration is invalid"},
		{name: "extra document", body: hubServePostgresConfig + "---\npostgres: {}\n", want: "hub configuration must contain one document"},
		{name: "partial hosted configuration", body: hubServePostgresConfig + "organization_id: org_example\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			cmd := newHubCommandWithRun("v-test", func(name string) string {
				if name == "TEST_HUB_POSTGRES_PASSWORD" {
					return "postgres-fixture-password"
				}
				return ""
			}, func(context.Context, hubserver.Config) error {
				called = true
				return nil
			})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"serve"}
			if test.body != "" {
				args = append(args, "--hosted-config", writeHubServeConfig(t, test.body))
			}
			cmd.SetArgs(append(args, test.flags...))
			err := cmd.ExecuteContext(t.Context())
			if called || err == nil || test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("serve called=%t error=%v, want validation failure %q", called, err, test.want)
			}
		})
	}
}

func TestHubServeCommandValidatesWebhookSecretEnvironmentName(t *testing.T) {
	t.Parallel()

	runErr := errors.New("runner should not be called")
	run := func(context.Context, hubserver.Config) error {
		return runErr
	}
	cmd := newHubCommandWithRun("v-test", func(string) string { return "" }, run)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"serve",
		"--hosted-config", writeHubServeConfig(t, hubServePostgresConfig),
		"--github-webhook-secret-env", "INVALID-NAME",
	})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Hub GitHub webhook secret environment variable name is invalid") {
		t.Fatalf("ExecuteContext() error = %v, want webhook environment validation failure", err)
	}
	if errors.Is(err, runErr) {
		t.Fatalf("ExecuteContext() error = %v, runner was called", err)
	}
}

func TestHubServeCommandValidatesAdministratorTokenEnvironment(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		env  string
		want string
	}{
		{name: "invalid environment variable", env: "INVALID-NAME", want: "Hub administrator token environment variable name is invalid"},
		{name: "unset token on restart", env: "TEST_HUB_ADMIN_TOKEN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			cmd := newHubCommandWithRun("v-test", func(name string) string {
				if name == "TEST_HUB_POSTGRES_PASSWORD" {
					return "postgres-fixture-password"
				}
				return ""
			}, func(_ context.Context, cfg hubserver.Config) error {
				called = true
				if cfg.Hosted != nil || len(cfg.InitialAdminToken) != 0 {
					t.Error("restart did not receive local mode with an empty bootstrap token")
				}
				return nil
			})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"serve", "--hosted-config", writeHubServeConfig(t, hubServePostgresConfig), "--admin-token-env", test.env})
			err := cmd.ExecuteContext(t.Context())
			if test.want == "" {
				if err != nil || !called {
					t.Fatalf("serve called=%t error=%v", called, err)
				}
			} else if called || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("serve called=%t error=%v, want %q", called, err, test.want)
			}
		})
	}
}

type hubContextKey struct{}
