package hubserver

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const (
	testTimestamp     = "2026-09-01T12:00:00Z"
	testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"
)

func TestOpenCreatesHubSchemaAndConfiguresSQLite(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		BusyTimeout:  2500 * time.Millisecond,
	})

	rows, err := service.database.db.QueryContext(t.Context(), "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatalf("query schema tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan schema table: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema tables: %v", err)
	}

	wantTables := []string{
		"ai_credit_accounts", "ai_credit_packs", "ai_credit_purchases", "ai_credit_transactions",
		"artifact_services",
		"artifact_references",
		"artifact_grants",
		"attachment_references",
		"attachments",
		"change_evidence",
		"change_issue_links",
		"change_landing_receipts",
		"change_requests",
		"change_review_policies",
		"change_sources",
		"change_versions",
		"change_viewed_files",
		"github_cutovers",
		"github_import_records",
		"github_imports",
		"collaboration_events",
		"collaboration_versions",
		"attempt_diff_body_migration",
		"attempt_diff_files",
		"attempt_diffs",
		"attempt_usage",
		"usage_cost_observations",
		"hub_identity",
		"hosted_tenant",
		"hosted_members",
		"hosted_project_grants",
		"hosted_sessions",
		"hosted_transactions",
		"hosted_invitations",
		"hosted_audit",
		"hosted_billing_accounts",
		"hosted_billing_audit",
		"hosted_billing_customer_intents",
		"hosted_billing_events",
		"hosted_billing_prices",
		"hosted_billing_retired",
		"hosted_binding_migrations",
		"hosted_plans",
		"hosted_plan_assignments",
		"hosted_complimentary_grants",
		"hosted_plan_audit",
		"hosted_usage_windows",
		"hosted_work_view_preferences",
		"hosted_member_reservations",
		"hosted_artifact_usage",
		"hosted_usage_counters",
		"native_commands",
		"native_comments",
		"native_issue_page_items",
		"native_issue_pages",
		"health_findings",
		"health_finding_issues",
		"health_detector_ticks",
		"health_slack_deliveries",
		"organization_secret_audit",
		"organization_secrets",
		"slack_integrations",
		"organizations",
		"projects",
		"token_grants",
		"api_tokens",
		"github_hydration_requests",
		"github_outbox",
		"github_webhook_inbox",
		"github_webhook_payloads",
		"hub_schema_version",
		"issue_dependencies",
		"issues",
		"landing_barrier_receipts",
		"landing_barriers",
		"leases",
		"lease_policies",
		"lease_runners",
		"monthly_budget_admissions",
		"monthly_budget_leases",
		"monthly_budget_policies",
		"policy_revisions",
		"project_action_runs",
		"project_actions",
		"project_observed_policies",
		"project_onboarding",
		"onboarding_issue_intake",
		"project_policies",
		"linked_issue_sources",
		"project_secrets",
		"project_secret_audit",
		"organization_sprite_pools",
		"organization_sprite_members",
		"project_workflow_applies",
		"provider_reservations",
		"machines",
		"native_attempts",
		"native_attempt_events",
		"pull_requests",
		"quality_landings",
		"queue_entries",
		"repositories",
		"runner_checkout_repositories",
		"runner_enrollment_projects",
		"runner_enrollments",
		"runner_identities",
		"runner_identity_events",
		"sync_checkpoints",
		"work_events",
		"workflow_states",
		"conversations",
		"conversation_messages",
		"conversation_questions",
		"conversation_commands",
		"conversation_events",
		"conversation_starts",
		"conversation_audience_events",
		"conversation_turn_batches",
		"conversation_prices",
		"conversation_usage",
		"conversation_attachments",
		"conversation_attachment_blobs",
		"operator_chat_sessions",
		"message_references",
		// Workspace sessions and the relay (decisions sections 18.1 and 18.2).
		"workspace_sessions",
		"workspace_items",
		"workspace_occupancy",
		"workspace_relay_tickets",
		"workspace_relay_sessions",
		"workspace_terminal_recordings",
	}
	sort.Strings(wantTables)
	if strings.Join(tables, ",") != strings.Join(wantTables, ",") {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}

	checks := []struct {
		name  string
		query string
		want  string
	}{
		{name: "journal mode", query: "PRAGMA journal_mode", want: "wal"},
		{name: "foreign keys", query: "PRAGMA foreign_keys", want: "1"},
		{name: "busy timeout", query: "PRAGMA busy_timeout", want: "2500"},
		{name: "locking mode", query: "PRAGMA locking_mode", want: "normal"},
		{name: "synchronous mode", query: "PRAGMA synchronous", want: "2"},
		{name: "application id", query: "PRAGMA application_id", want: strconv.Itoa(hubApplicationID)},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			var got string
			if err := service.database.db.QueryRowContext(t.Context(), check.query).Scan(&got); err != nil {
				t.Fatalf("query %s: %v", check.name, err)
			}
			if !strings.EqualFold(got, check.want) {
				t.Fatalf("%s = %q, want %q", check.name, got, check.want)
			}
		})
	}

	if service.database.schemaVersion != supportedSchemaVersion(t) {
		t.Fatalf("schema version = %d, want %d", service.database.schemaVersion, supportedSchemaVersion(t))
	}

	reader, err := sql.Open("sqlite", "file:"+filepath.ToSlash(service.database.path)+"?mode=ro&_pragma=busy_timeout(2500)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var applicationID int
	if err := reader.QueryRowContext(t.Context(), "PRAGMA application_id").Scan(&applicationID); err != nil || applicationID != hubApplicationID {
		t.Fatalf("live replication read = %d, %v; want %d while the Hub holds the database", applicationID, err, hubApplicationID)
	}
}

func TestOpenRejectsInvalidDatabasePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "memory", path: ":memory:"},
		{name: "URI", path: "file:hub.db"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, err := Open(t.Context(), Config{DatabasePath: test.path, Logger: discardLogger()})
			if err == nil {
				service.Close()
				t.Fatal("Open() error = nil, want validation error")
			}
		})
	}
}

func TestOpenRejectsNetworkFilesystemBeforeTakingOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()

	directory := t.TempDir()
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	path := filepath.Join(directory, "hub.db")
	service, err := Open(t.Context(), Config{
		DatabasePath: path,
		Logger:       discardLogger(),
		validateDatabaseFilesystem: func(gotDirectory string) error {
			if gotDirectory != resolvedDirectory {
				t.Fatalf("filesystem directory = %q, want %q", gotDirectory, resolvedDirectory)
			}
			return ErrNetworkFilesystem
		},
	})
	if err == nil {
		service.Close()
		t.Fatal("Open() error = nil, want network filesystem error")
	}
	if !errors.Is(err, ErrNetworkFilesystem) {
		t.Fatalf("Open() error = %v, want ErrNetworkFilesystem", err)
	}
	for _, candidate := range []string{path, path + ".lock"} {
		if _, statErr := os.Stat(candidate); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("os.Stat(%q) error = %v, want file not to exist", candidate, statErr)
		}
	}
}

func TestOpenCreatesPrivateDatabaseFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file modes")
	}

	path := filepath.Join(t.TempDir(), "hub.db")
	openTestService(t, Config{DatabasePath: path})

	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if errors.Is(err, os.ErrNotExist) && candidate != path {
			continue
		}
		if err != nil {
			t.Fatalf("os.Stat(%q) error = %v", candidate, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s permissions = %o, want 600", filepath.Base(candidate), got)
		}
	}
}

func TestSQLiteDSNEncodesAbsolutePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantPrefix string
	}{
		{name: "Unix", path: "/var/lib/detent/hub.db", wantPrefix: "file:///var/lib/detent/hub.db?"},
		{name: "Windows uppercase drive", path: "C:/detent/hub.db", wantPrefix: "file:///C:/detent/hub.db?"},
		{name: "Windows lowercase drive", path: "d:/detent/hub.db", wantPrefix: "file:///d:/detent/hub.db?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := sqliteDSN(test.path, defaultBusyTimeout)
			if !strings.HasPrefix(got, test.wantPrefix) {
				t.Fatalf("sqliteDSN() = %q, want prefix %q", got, test.wantPrefix)
			}
		})
	}
}

func TestOpenRejectsUnrecognizedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, *sql.DB)
	}{
		{
			name: "wrong application id",
			setup: func(t *testing.T, db *sql.DB) {
				if _, err := db.ExecContext(t.Context(), "PRAGMA application_id = 7"); err != nil {
					t.Fatalf("set application id: %v", err)
				}
			},
		},
		{
			name: "existing unrecognized table",
			setup: func(t *testing.T, db *sql.DB) {
				if _, err := db.ExecContext(t.Context(), "CREATE TABLE unrelated (id INTEGER PRIMARY KEY)"); err != nil {
					t.Fatalf("create unrelated table: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "database.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			test.setup(t, db)
			if err := db.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			service, err := Open(t.Context(), Config{DatabasePath: path, Logger: discardLogger()})
			if err == nil {
				service.Close()
				t.Fatal("Open() error = nil, want identity error")
			}
			if !errors.Is(err, ErrDatabaseIdentity) {
				t.Fatalf("Open() error = %v, want ErrDatabaseIdentity", err)
			}
		})
	}
}

func TestHubTransactionsWaitForConcurrentReplicationWriter(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), BusyTimeout: 5 * time.Second})
	db := service.database.db
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE _litestream_seq (id INTEGER PRIMARY KEY, seq INTEGER)"); err != nil {
		t.Fatal(err)
	}
	replicator, err := sql.Open("sqlite", "file:"+filepath.ToSlash(service.database.path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer replicator.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM _litestream_seq").Scan(&count); err != nil {
		t.Fatal(err)
	}
	replicated := make(chan error, 1)
	go func() {
		_, err := replicator.ExecContext(context.Background(), "INSERT INTO _litestream_seq (seq) VALUES (1)")
		replicated <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO _litestream_seq (seq) VALUES (2)"); err != nil {
		t.Fatalf("hub write after a concurrent replication write: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-replicated; err != nil {
		t.Fatalf("replication write: %v", err)
	}
}

func TestOpenAcceptsLitestreamTablesOnNewDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"CREATE TABLE _litestream_seq (id INTEGER PRIMARY KEY, seq INTEGER)", "CREATE TABLE _litestream_lock (id INTEGER)"} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	openTestService(t, Config{DatabasePath: path})
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hub.db")
	service := openTestService(t, Config{DatabasePath: path})
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO hub_schema_version (version_id, is_applied) VALUES (?, 1)", supportedSchemaVersion(t)+1); err != nil {
		t.Fatalf("insert future schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	service, err = Open(t.Context(), Config{DatabasePath: path, Logger: discardLogger()})
	if err == nil {
		service.Close()
		t.Fatal("Open() error = nil, want unsupported schema error")
	}
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Open() error = %v, want ErrUnsupportedSchema", err)
	}
}

func TestOpenMigratesLegacyWebhookPayloads(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatalf("set application id: %v", err)
	}
	legacyMigrations := fstest.MapFS{}
	for _, name := range []string{
		"00001_create_github_projection.sql",
		"00002_create_execution_state.sql",
		"00003_create_github_delivery.sql",
	} {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		legacyMigrations[name] = &fstest.MapFile{Data: data}
	}
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		legacyMigrations,
		goose.WithDisableGlobalRegistry(true),
		goose.WithTableName(hubSchemaTable),
		goose.WithSlog(discardLogger()),
	)
	if err != nil {
		t.Fatalf("create legacy migration provider: %v", err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatalf("apply legacy migrations: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO github_webhook_inbox (
			delivery_id, event_type, action, headers_json, payload_json,
			payload_sha256, payload_bytes, status, received_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "legacy-delivery", "push", "created", `{"user_agent":"GitHub-Hookshot"}`, `{"ref":"refs/heads/main"}`,
		"legacy-sha", 25, "pending", testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert legacy webhook: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	service := openTestService(t, Config{
		DatabasePath: path,
		now: func() time.Time {
			return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
		},
	})
	var eventType string
	var action string
	var headers string
	var body string
	var payloadSHA string
	var lastReceivedAt string
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT i.event_type, i.action, i.headers_json, CAST(p.body AS TEXT), i.payload_sha256, i.last_received_at
		FROM github_webhook_inbox i
		JOIN github_webhook_payloads p ON p.inbox_id = i.id
		WHERE i.delivery_id = 'legacy-delivery'
	`).Scan(&eventType, &action, &headers, &body, &payloadSHA, &lastReceivedAt); err != nil {
		t.Fatalf("read migrated webhook: %v", err)
	}
	if eventType != "push" || action != "created" || headers != `{"user_agent":"GitHub-Hookshot"}` ||
		body != `{"ref":"refs/heads/main"}` || payloadSHA != "legacy-sha" || lastReceivedAt != testTimestamp {
		t.Fatalf("migrated webhook = event %q action %q headers %s body %s sha %q received %q", eventType, action, headers, body, payloadSHA, lastReceivedAt)
	}
}

func TestOpenMigratesConversationOriginHistories(t *testing.T) {
	if testing.Short() {
		t.Skip("SQLite migration history integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name          string
		version       int
		originVersion int
	}{
		{"schema60", 60, 0},
		{"attachments62", 62, 0},
		{"origin62", 62, 62},
		{"origin63", 63, 63},
		{"references64", 64, 0},
		{"origin65", 65, 0},
		{"hostedEvents66", 66, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			db.SetMaxOpenConns(1)
			if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
				t.Fatal(err)
			}
			legacy := fstest.MapFS{}
			entries, err := migrationFiles.ReadDir("migrations")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
				if err != nil {
					t.Fatal(err)
				}
				if version > test.version || version == test.originVersion {
					continue
				}
				data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				legacy[entry.Name()] = &fstest.MapFile{Data: data}
			}
			if test.originVersion != 0 {
				data, err := os.ReadFile("testdata/legacy_conversation_origin.sql")
				if err != nil {
					t.Fatal(err)
				}
				legacy[fmt.Sprintf("%05d_conversation_origin.sql", test.originVersion)] = &fstest.MapFile{Data: data}
			}
			if test.version >= 64 {
				data, err := migrationFiles.ReadFile("migration_steps/00064_cloud_attachment_references.sql")
				if err != nil {
					t.Fatal(err)
				}
				legacy["00064_cloud_attachment_references.sql"] = &fstest.MapFile{Data: data}
			}
			goMigrations := []*goose.Migration{hubGoMigrations()[0]}
			if test.version >= 65 {
				goMigrations = append(goMigrations, hubGoMigrations()[2])
			}
			provider, err := goose.NewProvider(goose.DialectSQLite3, db, legacy,
				goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable),
				goose.WithSlog(discardLogger()), goose.WithGoMigrations(goMigrations...))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `
INSERT INTO organizations (id, name, created_at) VALUES ('org_history', 'History', 'now');
INSERT INTO projects (id, organization_id, name, profile, created_at) VALUES ('prj_history', 'org_history', 'History', 'native', 'now');
INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at)
VALUES ('tok_history', 'History', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'history', 'operator', 'now', 'now');
INSERT INTO conversations (id, organization_id, project_id, owner_principal_id, visibility, status, work_item_id, execution_json, created_at, updated_at)
VALUES ('conv_history', 'org_history', 'prj_history', 'tok_history', 'shared', 'active', 'wi_history', '{}', 'now', 'now');
`); err != nil {
				t.Fatal(err)
			}
			if test.originVersion != 0 {
				if _, err := db.ExecContext(t.Context(), "UPDATE conversations SET origin = 'user' WHERE id = 'conv_history'"); err != nil {
					t.Fatal(err)
				}
			}
			if test.version >= 62 && test.originVersion != 62 {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO attachments
(id, organization_id, project_id, uploader, name, content_type, size, sha256, created_at)
VALUES ('att_history', 'org_history', 'prj_history', 'tok_history', 'preserved.txt', 'text/plain', 1, 'hash', 'now')`); err != nil {
					t.Fatal(err)
				}
				if test.version >= 64 {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO attachment_references (attachment_id, work_item_id) VALUES ('att_history', 'wi_reference')"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			service := openTestService(t, Config{DatabasePath: path})
			if service.database.schemaVersion != supportedSchemaVersion(t) {
				t.Fatalf("schema version = %d, want %d", service.database.schemaVersion, supportedSchemaVersion(t))
			}
			var urgentUpdate string
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT urgent_runner_update_json FROM organizations WHERE id = 'org_history'").Scan(&urgentUpdate); err != nil || urgentUpdate != `{"revision":0,"request":null}` {
				t.Fatalf("initial urgent update = %q, %v", urgentUpdate, err)
			}
			for _, object := range []struct{ kind, name string }{
				{"table", "attachments"}, {"table", "attachment_references"}, {"index", "attachments_retention"},
				{"index", "collaboration_events_project_idx"},
				{"trigger", "attachments_issue_deleted"}, {"trigger", "attachments_comment_deleted"},
			} {
				var count int
				if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE type = ? AND name = ?", object.kind, object.name).Scan(&count); err != nil || count != 1 {
					t.Fatalf("schema object %s = %d, %v", object.name, count, err)
				}
			}
			var observation int
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM pragma_table_info('runner_identities') WHERE name = 'update_observation_json'").Scan(&observation); err != nil || observation != 1 {
				t.Fatalf("runner observation column = %d, %v", observation, err)
			}
			var origin string
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT origin FROM conversations WHERE id = 'conv_history'").Scan(&origin); err != nil {
				t.Fatal(err)
			}
			want := conversationOriginWorker
			if test.originVersion != 0 || test.version >= 65 {
				want = conversationOriginUser
			}
			if origin != want {
				t.Fatalf("conversation origin = %q, want %q", origin, want)
			}
			if test.version >= 62 && test.originVersion != 62 {
				var name string
				if err := service.database.db.QueryRowContext(t.Context(), "SELECT name FROM attachments WHERE id = 'att_history'").Scan(&name); err != nil || name != "preserved.txt" {
					t.Fatalf("preserved attachment = %q, %v", name, err)
				}
				if test.version >= 64 {
					var workItem string
					if err := service.database.db.QueryRowContext(t.Context(), "SELECT work_item_id FROM attachment_references WHERE attachment_id = 'att_history'").Scan(&workItem); err != nil || workItem != "wi_reference" {
						t.Fatalf("preserved attachment reference = %q, %v", workItem, err)
					}
				}
			}
			if _, err := runMigrations(t.Context(), service.database.db, discardLogger()); err != nil {
				t.Fatalf("repeat unchanged-schema startup: %v", err)
			}
		})
	}
}

func TestSchemaEnforcesIdentityAndAppendOnlyConstraints(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	db := service.database.db
	repositoryID, issueID := seedProjection(t, db)

	duplicateTests := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "repository GitHub node id",
			query: "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			args:  []any{"R_repo", "other", "repo", testTimestamp, testTimestamp},
		},
		{
			name:  "issue GitHub node id",
			query: "INSERT INTO issues (repository_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			args:  []any{repositoryID, "I_issue", 2, "Other", "https://example.test/2", "open", "v2", testTimestamp, testTimestamp, testTimestamp, testTimestamp},
		},
		{
			name:  "workflow state GitHub node id",
			query: "INSERT INTO workflow_states (repository_id, github_node_id, source_name, detent_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			args:  []any{repositoryID, "WS_todo", "Other", "Other", testTimestamp, testTimestamp},
		},
		{
			name:  "pull request GitHub node id",
			query: "INSERT INTO pull_requests (repository_id, github_node_id, github_number, title, url, github_state, head_ref, head_sha, base_ref, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			args:  []any{repositoryID, "PR_one", 2, "Other", "https://example.test/pr/2", "open", "other", "def", "main", "v2", testTimestamp, testTimestamp, testTimestamp, testTimestamp},
		},
		{
			name:  "webhook delivery id",
			query: "INSERT INTO github_webhook_inbox (delivery_id, event_type, payload_sha256, payload_bytes, status, received_at, last_received_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			args:  []any{"delivery-one", "issues", "def", 2, "pending", testTimestamp, testTimestamp, testTimestamp, testTimestamp},
		},
	}
	for _, test := range duplicateTests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), test.query, test.args...); err == nil {
				t.Fatal("duplicate insert error = nil, want unique constraint error")
			}
		})
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO github_hydration_requests (
			repository_id, repository_full_name, object_kind, object_key,
			github_number, reason, requested_source_version,
			first_delivery_id, last_delivery_id, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, repositoryID, "digitaldrywood/detent", "repository", "detent", 1, "invalid", "v1",
		"delivery-one", "delivery-one", "pending", testTimestamp, testTimestamp); err == nil {
		t.Fatal("invalid hydration object kind error = nil, want constraint error")
	}

	if _, err := db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (9999, 'I_missing', 9, 'Missing', 'https://example.test/9', 'open', 'v1', ?, ?, ?, ?)", testTimestamp, testTimestamp, testTimestamp, testTimestamp); err == nil {
		t.Fatal("foreign key insert error = nil, want constraint error")
	}

	if _, err := db.ExecContext(t.Context(), "INSERT INTO machines (id, hostname, capacity, version, last_heartbeat_at, registered_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", "machine-one", "builder", 2, "v1", testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	firstLease, err := db.ExecContext(t.Context(), "INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", "lease-one", issueID, "machine-one", "session-one", testTimestamp, testTimestamp, testTimestamp, testTimestamp, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert first lease: %v", err)
	}
	firstToken, err := firstLease.LastInsertId()
	if err != nil {
		t.Fatalf("first lease token: %v", err)
	}
	secondLease, err := db.ExecContext(t.Context(), "INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", "lease-two", issueID, "machine-one", "session-two", testTimestamp, testTimestamp, testTimestamp, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert second lease: %v", err)
	}
	secondToken, err := secondLease.LastInsertId()
	if err != nil {
		t.Fatalf("second lease token: %v", err)
	}
	if secondToken <= firstToken {
		t.Fatalf("second fencing token = %d, want greater than %d", secondToken, firstToken)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", "lease-three", issueID, "machine-one", "session-three", testTimestamp, testTimestamp, testTimestamp, testTimestamp, testTimestamp); err == nil {
		t.Fatal("second active lease error = nil, want unique constraint error")
	}

	event, err := db.ExecContext(t.Context(), "INSERT INTO work_events (issue_id, fencing_token, machine_id, session_id, kind, payload_json, occurred_at, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", issueID, secondToken, "machine-one", "session-two", "progress", "{}", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert work event: %v", err)
	}
	eventID, err := event.LastInsertId()
	if err != nil {
		t.Fatalf("work event id: %v", err)
	}
	for _, mutation := range []string{
		"UPDATE work_events SET kind = 'changed' WHERE id = ?",
		"DELETE FROM work_events WHERE id = ?",
	} {
		if _, err := db.ExecContext(t.Context(), mutation, eventID); err == nil {
			t.Fatalf("%s error = nil, want append-only constraint error", mutation)
		}
	}
}

func TestOpenExcludesAnotherHubProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	for _, test := range []struct {
		name                    string
		shared, cancel, sibling bool
	}{
		{name: "standalone refuses contention"},
		{name: "shared entry waits for previous owner", shared: true},
		{name: "shared entry cancels ownership wait", shared: true, cancel: true},
		{name: "shared entry refuses live sibling", shared: true, sibling: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.db")
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHubOwnerHelperProcess$")
			command.Env = append(os.Environ(), "DETENT_HUB_OWNER_HELPER="+path, "GOCOVERDIR="+t.TempDir())
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				_ = stdin.Close()
				if !waited {
					_ = command.Wait()
				}
			})
			ready, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || strings.TrimSpace(ready) != "ready" {
				t.Fatalf("helper readiness = %q: %v", ready, err)
			}

			if test.sibling {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				contender := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHubOwnerHelperProcess$")
				contender.Env = append(os.Environ(), "DETENT_HUB_OWNER_HELPER="+path, "DETENT_HUB_OWNER_CONTENDER=1", "GOCOVERDIR="+t.TempDir())
				output, err := contender.CombinedOutput()
				if err != nil || !strings.Contains(string(output), "held\n") {
					t.Fatalf("live sibling contender = %v, %s", err, output)
				}
				return
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := Config{DatabasePath: path, Logger: discardLogger()}
			waiting := make(chan struct{}, 1)
			if test.shared {
				cfg = hostedSharedTestConfig(path, newHostedSecurityProvider(), hostedSharedKey(7), 1)
				cfg.Logger = slog.New(databaseLockWaitHandler{Handler: slog.DiscardHandler, waiting: waiting})
			}
			type opened struct {
				service *Service
				err     error
			}
			result := make(chan opened, 1)
			go func() {
				service, err := Open(ctx, cfg)
				result <- opened{service: service, err: err}
			}()
			if test.shared {
				select {
				case <-waiting:
				case got := <-result:
					if got.service != nil {
						_ = got.service.Close()
					}
					t.Fatalf("shared tenant exited instead of waiting: %v", got.err)
				case <-time.After(20 * time.Second):
					t.Fatal("tenant did not wait on previous owner")
				}
				if test.cancel {
					cancel()
				} else {
					_ = stdin.Close()
				}
			}
			var got opened
			select {
			case got = <-result:
			case <-time.After(20 * time.Second):
				t.Fatal("tenant startup did not finish")
			}
			if got.service != nil {
				t.Cleanup(func() { _ = got.service.Close() })
			}
			switch {
			case !test.shared:
				if !errors.Is(got.err, instancelock.ErrHeld) {
					t.Fatalf("Open() = %v, want held", got.err)
				}
			case test.cancel:
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("Open() = %v, want cancellation", got.err)
				}
			default:
				if got.err != nil {
					t.Fatalf("Open() after holder exit = %v", got.err)
				}
				if err := got.service.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Fatal(err)
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("holder exit = %v, %s", err, stderr.String())
			}
			waited = true
			service := openTestService(t, cfg)
			if service.database.path == "" {
				t.Fatal("reopened database path is empty")
			}
			if test.shared {
				_, err := Open(t.Context(), cfg)
				if !errors.Is(err, instancelock.ErrHeld) {
					t.Fatalf("same-process contender = %v, want held", err)
				}
			}
		})
	}
}

type databaseLockWaitHandler struct {
	slog.Handler
	waiting chan struct{}
}

func (h databaseLockWaitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h databaseLockWaitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "waiting for previous tenant Hub database ownership" {
		select {
		case h.waiting <- struct{}{}:
		default:
		}
	}
	return h.Handler.Handle(ctx, record)
}

func TestHubOwnerHelperProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	path := os.Getenv("DETENT_HUB_OWNER_HELPER")
	if path == "" {
		return
	}
	if os.Getenv("DETENT_HUB_OWNER_CONTENDER") != "" {
		cfg := hostedSharedTestConfig(path, newHostedSecurityProvider(), hostedSharedKey(7), 1)
		service, err := Open(t.Context(), cfg)
		if service != nil {
			_ = service.Close()
		}
		if !errors.Is(err, instancelock.ErrHeld) {
			t.Fatalf("live sibling contention = %v", err)
		}
		fmt.Fprintln(os.Stdout, "held")
		return
	}

	service, err := Open(t.Context(), Config{DatabasePath: path, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatalf("write readiness: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for parent: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOnlineBackupPreservesSchemaAndData(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	repositoryID, _ := seedProjection(t, service.database.db)
	backupPath := filepath.Join(t.TempDir(), "backups", "hub.db")
	if err := service.Backup(t.Context(), backupPath); err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	if err := service.database.health(t.Context()); err != nil {
		t.Fatalf("source health after backup: %v", err)
	}

	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() {
		if err := backup.Close(); err != nil {
			t.Fatalf("close backup: %v", err)
		}
	})
	var integrity string
	if err := backup.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("backup integrity check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("backup integrity = %q, want ok", integrity)
	}
	var gotRepositoryID int64
	if err := backup.QueryRowContext(t.Context(), "SELECT id FROM repositories WHERE github_node_id = 'R_repo'").Scan(&gotRepositoryID); err != nil {
		t.Fatalf("query backed up repository: %v", err)
	}
	if gotRepositoryID != repositoryID {
		t.Fatalf("backed up repository id = %d, want %d", gotRepositoryID, repositoryID)
	}
	version, err := currentSchemaVersion(t.Context(), backup)
	if err != nil {
		t.Fatalf("backup schema version: %v", err)
	}
	if version != supportedSchemaVersion(t) {
		t.Fatalf("backup schema version = %d, want %d", version, supportedSchemaVersion(t))
	}
	var applicationID int64
	if err := backup.QueryRowContext(t.Context(), "PRAGMA application_id").Scan(&applicationID); err != nil {
		t.Fatalf("backup application id: %v", err)
	}
	if applicationID != hubApplicationID {
		t.Fatalf("backup application id = %d, want %d", applicationID, hubApplicationID)
	}
}

func TestBackupRejectsUnsafeDestinations(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	if err := service.Backup(t.Context(), service.database.path); !errors.Is(err, ErrBackupSource) {
		t.Fatalf("Backup(source) error = %v, want ErrBackupSource", err)
	}
	existing := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(existing, []byte("preserve"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := service.Backup(t.Context(), existing); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Backup(existing) error = %v, want os.ErrExist", err)
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "preserve" {
		t.Fatalf("existing backup content = %q, want preserve", content)
	}
}

func TestBackupCancellationRemovesPartialDestination(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	destination := filepath.Join(t.TempDir(), "backup.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Backup(ctx, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("Backup() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup destination Stat() error = %v, want os.ErrNotExist", err)
	}
}

func openTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.RunnerReleaseClient == nil {
		cfg.RunnerReleaseClient = &runnerReleaseFixture{}
	}
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	started := time.Now()
	defer func() { t.Logf("hub_fixture_open_seconds=%.6f", time.Since(started).Seconds()) }()
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	if len(cfg.InitialAdminToken) == 0 {
		cfg.InitialAdminToken = []byte(testHubAdminToken)
	}
	seedHubDatabaseTemplate(t, cfg.DatabasePath)
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return service
}

func authorizeHubTestRequest(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
}

func seedProjection(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	repository, err := db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)", "R_repo", "digitaldrywood", "detent", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	repositoryID, err := repository.LastInsertId()
	if err != nil {
		t.Fatalf("repository id: %v", err)
	}
	seedCompatibilityProject(t, db, repositoryID)
	workflow, err := db.ExecContext(t.Context(), "INSERT INTO workflow_states (repository_id, github_node_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)", repositoryID, "WS_todo", "Todo", "Todo", testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert workflow state: %v", err)
	}
	workflowID, err := workflow.LastInsertId()
	if err != nil {
		t.Fatalf("workflow state id: %v", err)
	}
	issue, err := db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, workflow_state_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", repositoryID, workflowID, "I_issue", 1, "Issue", "https://example.test/1", "open", "v1", testTimestamp, testTimestamp, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	issueID, err := issue.LastInsertId()
	if err != nil {
		t.Fatalf("issue id: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO pull_requests (repository_id, issue_id, github_node_id, github_number, title, url, github_state, head_ref, head_sha, base_ref, source_version, source_updated_at, synchronized_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", repositoryID, issueID, "PR_one", 1, "PR", "https://example.test/pr/1", "open", "feature", "abc", "main", "v1", testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert pull request: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO github_webhook_inbox (delivery_id, repository_id, event_type, payload_sha256, payload_bytes, status, received_at, last_received_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", "delivery-one", repositoryID, "issues", "abc", 2, "pending", testTimestamp, testTimestamp, testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert webhook delivery: %v", err)
	}
	return repositoryID, issueID
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedCompatibilityProject(t *testing.T, db *sql.DB, repositoryID int64) {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='projects')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO projects(organization_id,repository_id,name,profile,created_at)
SELECT o.id,r.id,r.github_owner || '/' || r.github_name,'github_compatible',r.created_at FROM organizations o CROSS JOIN repositories r WHERE o.local=1 AND r.id=?
ON CONFLICT(repository_id) DO NOTHING`, repositoryID); err != nil {
		t.Fatalf("seed compatibility project: %v", err)
	}
}

func TestReaderPoolReadsBesideHeldConnections(t *testing.T) {
	t.Parallel()

	type read struct {
		name string
		run  func(context.Context, nativeFixture, nativeScope, string) error
	}
	health := read{"health", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		if response, status := f.service.readInstanceHealth(ctx); status != http.StatusOK {
			return fmt.Errorf("health = %d %q", status, response.Status)
		}
		return nil
	}}
	internalHealth := read{"internal health", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		return f.service.database.health(ctx)
	}}
	detail := read{"work item detail", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, _, err := readNativeIssue(ctx, f.service.database.reader, scope, item)
		return err
	}}
	attempts := read{"attempts", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, err := f.service.readAttempts(ctx, scope, item, url.Values{})
		return err
	}}
	changes := read{"changes", func(ctx context.Context, f nativeFixture, scope nativeScope, item string) error {
		_, err := f.service.readChanges(ctx, scope, item)
		return err
	}}
	findings := read{"health findings", func(ctx context.Context, f nativeFixture, scope nativeScope, _ string) error {
		_, err := f.service.readHealthFindings(ctx, scope, "", "", "", 10)
		return err
	}}
	capabilities := read{"capabilities", func(ctx context.Context, f nativeFixture, _ nativeScope, _ string) error {
		_, err := f.service.readNativeCapabilities(ctx)
		return err
	}}
	board := read{"board", func(ctx context.Context, f nativeFixture, scope nativeScope, _ string) error {
		page, err := f.service.readIssues(ctx, scope, url.Values{"limit": {"1"}})
		if err == nil && page.Total != 2 {
			err = fmt.Errorf("board total = %d, want 2", page.Total)
		}
		return err
	}}

	tests := []struct {
		name  string
		hold  func(*testing.T, *Service) *sql.Tx
		reads []read
		write bool
	}{
		{
			name: "writer holds a write transaction",
			hold: func(t *testing.T, s *Service) *sql.Tx {
				tx, err := s.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET name = name"); err != nil {
					t.Fatal(err)
				}
				return tx
			},
			reads: []read{health, internalHealth, detail, attempts, changes, findings, capabilities},
			write: true,
		},
		{
			name: "reader pool holds a long read",
			hold: func(t *testing.T, s *Service) *sql.Tx {
				tx, err := s.database.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				var count int
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&count); err != nil {
					t.Fatal(err)
				}
				return tx
			},
			reads: []read{health, internalHealth, detail, attempts, changes, findings, capabilities, board},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "first")
			f.create(t, "second")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			item := string(issue.WorkItemID)

			held := test.hold(t, f.service)
			defer func() { _ = held.Rollback() }()
			for _, read := range test.reads {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				started := time.Now()
				err := read.run(ctx, f, scope, item)
				cancel()
				if err != nil {
					t.Fatalf("%s beside a held connection: %v after %s", read.name, err, time.Since(started))
				}
			}

			if _, err := f.service.database.reader.ExecContext(t.Context(), "UPDATE projects SET name = name"); err == nil {
				t.Fatal("reader pool accepted a write")
			}
			if !test.write {
				return
			}
			blocked, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if _, err := f.service.database.db.ExecContext(blocked, "UPDATE projects SET name = name"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("second write beside a held write = %v, want it to wait for the writer", err)
			}
			if err := held.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET name = name"); err != nil {
				t.Fatalf("write after the held write committed: %v", err)
			}
		})
	}
}

func TestHostedWriterLeavesCheckpointsToLitestream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		hosted          bool
		want            int
		wantSynchronous int
	}{
		{name: "hosted tenant", hosted: true, want: 0, wantSynchronous: 1},
		{name: "self-managed Hub", hosted: false, want: 1000, wantSynchronous: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", sqliteWriterDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout, test.hosted))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			var got int
			if err := db.QueryRowContext(t.Context(), "PRAGMA wal_autocheckpoint").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("wal_autocheckpoint = %d, want %d", got, test.want)
			}
			var synchronous int
			if err := db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
				t.Fatal(err)
			}
			if synchronous != test.wantSynchronous {
				t.Fatalf("synchronous = %d, want %d", synchronous, test.wantSynchronous)
			}
		})
	}
}
