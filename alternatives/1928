package hubserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRuntimeMigrationPreservesHistory(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "runtime-migration")
	issue := f.create(t, "retained history")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	var before tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil), &before)
	// Exercise migration 59 alone. Current fixtures include forward-only migrations
	// whose deployment rollback requires a binary supporting their schema version.
	data, err := migrationFiles.ReadFile("migrations/00059_native_runtime_evidence.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{"00059_native_runtime_evidence.sql": &fstest.MapFile{Data: data}}
	const versionTable = "runtime_migration_test_version"
	provider, err := goose.NewProvider(goose.DialectSQLite3, f.service.database.db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(versionTable), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.GetVersions(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The fixture's event table already has migration 59's shape. Record only
	// that migration in the isolated ledger, leaving the real schema ledger intact.
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO "+versionTable+" (version_id,is_applied) VALUES (59,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 58); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var after tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil), &after)
	if len(after.Items) != 1 || len(before.Items) != 1 || after.Items[0].ID != before.Items[0].ID || after.Items[0].Actor != before.Items[0].Actor || after.Items[0].RecordedAt != before.Items[0].RecordedAt {
		t.Fatalf("migration changed retained history: %#v -> %#v", before, after)
	}
	for _, statement := range []string{"UPDATE collaboration_events SET type='issue.edited' WHERE id=?", "DELETE FROM collaboration_events WHERE id=?"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement, before.Items[0].ID); err == nil {
			t.Fatal("migration lost append-only history enforcement")
		}
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "runtime-worker")
	lease := claimNativeAttempt(t, f, worker, "migration-machine", "migration-session", issue.WorkItemID)
	started := nativeStartedEvent(lease)
	started.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: f.service.config.now()}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, started), http.StatusOK)
	observed := started
	observed.Type, observed.IdempotencyKey, observed.Data.Sequence = "run.observed", "migration-observed", 2
	runtime := *started.Data.Runtime
	runtime.Phase = "validation"
	observed.Data.Runtime = &runtime
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, observed), http.StatusOK)
	if _, err := provider.DownTo(t.Context(), 58); err == nil {
		t.Fatal("rollback discarded native runtime evidence")
	}
	var version int64
	version, _, err = provider.GetVersions(t.Context())
	if err != nil || version != 59 {
		t.Fatalf("failed rollback changed schema: %d %v", version, err)
	}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil), &after)
	if len(after.Items) != 4 || after.Items[0].ID != before.Items[0].ID || after.Items[1].Type != "scheduler.decision" || after.Items[2].Type != "run.started" || after.Items[3].Type != "run.observed" {
		t.Fatalf("failed rollback changed evidence: %#v", after)
	}
	t.Run("activity backfill", func(t *testing.T) {
		f := newPullRequestFixture(t, true)
		worker := f.worker(t, "activity-migration-worker")
		lease := claimNativeAttempt(t, f.nativeFixture, worker, "activity-machine", "activity-session", f.issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, nativeStartedEvent(lease)), http.StatusOK)
		version := f.publishExternal(t, "activity-version")
		for _, body := range []string{"Comment", "## Codex Workpad"} {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: body}, Body: body}), http.StatusOK)
		}
		const file = "20261005174814_native_issue_activity.sql"
		data, err := migrationFiles.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		const ledger = "activity_migration_test_version"
		provider, err := goose.NewProvider(goose.DialectSQLite3, f.service.database.db, fstest.MapFS{file: &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithTableName(ledger), goose.WithSlog(discardLogger()))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := provider.GetVersions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO "+ledger+" (version_id,is_applied) VALUES (20261005174814,1)"); err != nil {
			t.Fatal(err)
		}
		for index, test := range []struct{ name, query string }{
			{"updated_at fallback", ""},
			{"issue edit", "UPDATE issues SET native_updated_at = :at WHERE native_id = :issue"},
			{"comment", "UPDATE native_comments SET updated_at = :at WHERE work_item_id = :issue AND body = 'Comment'"},
			{"Workpad", "UPDATE native_comments SET updated_at = :at WHERE work_item_id = :issue AND body = '## Codex Workpad'"},
			{"run start", "UPDATE native_attempts SET started_at = :at WHERE work_item_id = :issue"},
			{"run finish", "UPDATE native_attempts SET updated_at = :at, status = 'succeeded' WHERE work_item_id = :issue"},
			{"history", "INSERT INTO collaboration_events (id, organization_id, project_id, work_item_id, sequence, type, schema_version, actor_json, data_json, recorded_at) SELECT 'activity-history', organization_id, project_id, native_id, (SELECT max(sequence) + 1 FROM collaboration_events WHERE work_item_id = :issue), 'run.finished', 1, actor_json, '{}', :at FROM issues WHERE native_id = :issue"},
			{"legacy event", "INSERT INTO work_events (issue_id, fencing_token, kind, payload_json, occurred_at, recorded_at) SELECT id, 1, 'activity-event', '{}', :at, :at FROM issues WHERE native_id = :issue"},
			{"change version", "INSERT INTO change_versions (id, change_id, number, record_json) VALUES ('activity-version', :change, 2, json_object('created_at', :at))"},
			{"change review", "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (:change, :version, 'review', json_object('created_at', :at))"},
			{"change check", "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (:change, :version, 'check', json_object('received_at', :at, 'completed_at', '2020-01-01T00:00:00Z'))"},
			{"change discussion", "INSERT INTO change_evidence (change_id, kind, record_json) VALUES (:change, 'discussion', json_object('created_at', :at))"},
			{"change landing", "UPDATE change_requests SET record_json = json_set(record_json, '$.updated_at', :at) WHERE id = :change"},
			{"linked PR", "UPDATE pull_requests SET updated_at = :at WHERE repository_id = :repository"},
			{"external PR", "UPDATE pull_requests SET issue_id = NULL, synchronized_at = :at WHERE repository_id = :repository"},
		} {
			t.Run(test.name, func(t *testing.T) {
				if _, err := provider.DownTo(t.Context(), 0); err != nil {
					t.Fatal(err)
				}
				stamp := f.now.Add(time.Duration(index) * time.Nanosecond)
				if test.query != "" {
					var args []any
					for _, arg := range []sql.NamedArg{
						sql.Named("at", formatHubTime(stamp)),
						sql.Named("issue", f.issue.WorkItemID),
						sql.Named("change", f.change.ID),
						sql.Named("version", version.ID),
						sql.Named("repository", f.repositoryID),
					} {
						if strings.Contains(test.query, ":"+arg.Name) {
							args = append(args, arg)
						}
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), test.query, args...); err != nil {
						t.Fatal(err)
					}
				}
				var updated string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT native_updated_at FROM issues WHERE native_id = ?", f.issue.WorkItemID).Scan(&updated); err != nil {
					t.Fatal(err)
				}
				if _, err := provider.Up(t.Context()); err != nil {
					t.Fatal(err)
				}
				issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
				if !issue.LastActivityAt.Equal(stamp) || formatHubTime(issue.UpdatedAt) != updated {
					t.Fatalf("backfill activity = %s, updated = %s; want %s, %s", issue.LastActivityAt, issue.UpdatedAt, stamp, updated)
				}
				var missing, violations int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE last_activity_at IS NULL OR last_activity_at = ''").Scan(&missing); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
					t.Fatal(err)
				}
				if missing != 0 || violations != 0 {
					t.Fatalf("backfill left %d missing timestamps, %d foreign key violations", missing, violations)
				}
			})
		}
	})
}

func TestHubMigrationPreservesExistingData(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name           string
		version        int64
		hostedSessions string
		viewed         bool
		onboarding     bool
		crlf           bool
	}{
		{"selected operator keys", 55, "1", true, true, false},
		{"current selected operator keys", 67, "1", true, true, false},
		{"artifacts", 16, "0", false, false, false},
		{"hosted identity", 17, "1", false, false, false},
		{"viewed files version 18", 18, "1", true, false, false},
		{"onboarding version 18", 18, "1", false, true, false},
		{"both tables version 18", 18, "1", true, true, false},
		{"both tables CRLF version 18", 18, "1", true, true, true},
		{"repaired onboarding version 19", 19, "1", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hub.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
				t.Fatal(err)
			}
			files, err := migrationFiles.ReadDir("migrations")
			if err != nil {
				t.Fatal(err)
			}
			migrations := fstest.MapFS{}
			for _, file := range files {
				if test.version == 19 && file.Name() == "00019_project_onboarding.sql" {
					data, err := migrationFiles.ReadFile("migrations/" + file.Name())
					if err != nil {
						t.Fatal(err)
					}
					migrations[file.Name()] = &fstest.MapFile{Data: data}
					continue
				}
				if file.Name() >= "00018_" && file.Name() != "00018_change_viewed_files.sql" && test.version < 55 {
					continue
				}
				if file.Name() == "00018_change_viewed_files.sql" && !test.viewed {
					continue
				}
				data, err := migrationFiles.ReadFile("migrations/" + file.Name())
				if err != nil {
					t.Fatal(err)
				}
				migrations[file.Name()] = &fstest.MapFile{Data: data}
			}
			if test.onboarding && test.version == 18 {
				data, err := os.ReadFile("testdata/migrations/00018_project_onboarding.sql")
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
				if test.crlf {
					data = []byte(strings.ReplaceAll(string(data), "\n", "\r\n"))
				}
				if test.viewed {
					up, _, _ := strings.Cut(string(data), "-- +goose Down")
					up = strings.TrimPrefix(up, "-- +goose Up")
					viewed := migrations["00018_change_viewed_files.sql"]
					viewed.Data = append(viewed.Data, []byte(up)...)
				} else {
					migrations["00018_project_onboarding.sql"] = &fstest.MapFile{Data: data}
				}
			}
			provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable), goose.WithSlog(discardLogger()), goose.WithGoMigrations(hubGoMigrations()...))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.UpTo(t.Context(), 7); err != nil {
				t.Fatal(err)
			}
			_, issueID := seedProjection(t, db)
			if _, err := provider.UpTo(t.Context(), test.version); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens(id,name,scope,token_hash,token_fingerprint,created_at,updated_at,native_only) VALUES ('legacy-key','legacy key','operator','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','legacy-fingerprint','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',1)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO token_grants(token_id,organization_id,project_id) SELECT 'legacy-key',organization_id,project_id FROM issues WHERE id=?`, issueID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO change_requests (id, organization_id, project_id, work_item_id, record_json)
				SELECT 'existing-change', organization_id, project_id, native_id, '{}' FROM issues WHERE id = ?`, issueID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO change_versions (id, change_id, number, record_json) VALUES ('existing-version', 'existing-change', 1, '{"head_sha":"preserved"}')`); err != nil {
				t.Fatal(err)
			}
			if test.version >= 17 {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_sessions (token_hash, email, identity_json, expires_at, created_at) VALUES ('existing-session', 'reviewer@example.test', '{}', ?, ?)`, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			if test.viewed {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO change_viewed_files (version_id, principal_id, manifest_sha256, file_sha256, viewed) VALUES ('existing-version', 'reviewer', ?, ?, 1)`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			}
			if test.onboarding {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO project_onboarding (organization_id, project_id, revision, progress_json, updated_at) SELECT organization_id, project_id, 7, '{"policy_approved":true}', ? FROM issues WHERE id = ?`, testTimestamp, issueID); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := Config{DatabasePath: path}
			service := openTestService(t, cfg)
			if !test.viewed {
				if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO change_viewed_files (version_id, principal_id, manifest_sha256, file_sha256, viewed) VALUES ('existing-version', 'reviewer', ?, ?, 1)`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			}
			if !test.onboarding {
				if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO project_onboarding (organization_id, project_id, revision, progress_json, updated_at) SELECT organization_id, project_id, 7, '{"policy_approved":true}', ? FROM issues WHERE id = ?`, testTimestamp, issueID); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			service = openTestService(t, cfg)
			for _, check := range []struct{ name, query, want string }{
				{"legacy key access", "SELECT operator_project_access FROM api_tokens WHERE id='legacy-key'", "selected"},
				{"legacy key grants", "SELECT count(*) FROM token_grants g JOIN issues i ON i.organization_id=g.organization_id AND i.project_id=g.project_id WHERE g.token_id='legacy-key'", "1"},
				{"schema version", "SELECT max(version_id) FROM hub_schema_version WHERE is_applied = 1", strconv.FormatInt(supportedSchemaVersion(t), 10)},
				{"onboarding migration", "SELECT count(*) FROM hub_schema_version WHERE version_id = 19 AND is_applied = 1", "1"},
				{"allowance migration", "SELECT count(*) FROM hub_schema_version WHERE version_id = 20 AND is_applied = 1", "1"},
				{"allowance records", "SELECT count(*) FROM hosted_plan_assignments", "0"},
				{"onboarding progress", "SELECT progress_json FROM project_onboarding", `{"policy_approved":true}`},
				{"onboarding revision", "SELECT revision FROM project_onboarding", "7"},
				{"onboarding timestamp", "SELECT updated_at FROM project_onboarding", testTimestamp},
				{"identity migration", "SELECT count(*) FROM hub_schema_version WHERE version_id = 17 AND is_applied = 1", "1"},
				{"version 18 recorded once", "SELECT count(*) FROM hub_schema_version WHERE version_id = 18 AND is_applied = 1", "1"},
				{"existing code version", "SELECT record_json FROM change_versions WHERE id = 'existing-version'", `{"head_sha":"preserved"}`},
				{"viewed state", "SELECT viewed FROM change_viewed_files WHERE version_id = 'existing-version' AND principal_id = 'reviewer'", "1"},
				{"hosted sessions", "SELECT count(*) FROM hosted_sessions WHERE token_hash = 'existing-session' AND email = 'reviewer@example.test'", test.hostedSessions},
				{"foreign keys", "SELECT count(*) FROM pragma_foreign_key_check", "0"},
				{"foreign key enforcement", "PRAGMA foreign_keys", "1"},
				{"integrity", "PRAGMA integrity_check", "ok"},
			} {
				t.Run(check.name, func(t *testing.T) {
					var got string
					if err := service.database.db.QueryRowContext(t.Context(), check.query).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != check.want {
						t.Fatalf("got %q, want %q", got, check.want)
					}
				})
			}
		})
	}
}

func TestNativeMigrationPreservesCompatibilityIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", hubApplicationID)); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "00008_" {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		migrations[file.Name()] = &fstest.MapFile{Data: data}
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName(hubSchemaTable), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	repositoryID, issueID := seedProjection(t, db)
	for _, statement := range []string{
		`INSERT INTO machines (id, hostname, capacity, version, last_heartbeat_at, registered_at, updated_at) VALUES ('legacy-machine', 'host', 1, 'test', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`,
		fmt.Sprintf(`INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES ('legacy-lease', %d, 'legacy-machine', 'legacy-session', '2026-09-01T12:10:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`, issueID),
		fmt.Sprintf(`INSERT INTO work_events (issue_id, fencing_token, kind, payload_json, occurred_at, recorded_at) VALUES (%d, 1, 'legacy-event', '{"reference":"preserved"}', '2026-09-01T12:00:00Z', '2026-09-01T12:00:00Z')`, issueID),
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := openTestService(t, Config{DatabasePath: path})
	var nativeID, organizationID, projectID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT native_id, organization_id, project_id FROM issues WHERE id = ?", issueID).Scan(&nativeID, &organizationID, &projectID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, query, want string }{
		{"repository", fmt.Sprintf("SELECT repository_id FROM issues WHERE id = %d", issueID), strconv.FormatInt(repositoryID, 10)},
		{"GitHub identity", fmt.Sprintf("SELECT github_node_id FROM issues WHERE id = %d", issueID), "I_issue"},
		{"number", fmt.Sprintf("SELECT number FROM issues WHERE id = %d", issueID), "1"},
		{"compatibility profile", "SELECT profile FROM projects WHERE id = '" + projectID + "'", "github_compatible"},
		{"lease", "SELECT issue_id FROM leases WHERE lease_id = 'legacy-lease'", strconv.FormatInt(issueID, 10)},
		{"fencing", "SELECT fencing_token FROM leases WHERE lease_id = 'legacy-lease'", "1"},
		{"history", "SELECT payload_json FROM work_events WHERE kind = 'legacy-event'", `{"reference":"preserved"}`},
		{"foreign keys", "SELECT count(*) FROM pragma_foreign_key_check", "0"},
		{"foreign key enforcement", "PRAGMA foreign_keys", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got string
			if err := service.database.db.QueryRowContext(t.Context(), test.query).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_id = 'changed' WHERE id = ?", issueID); err == nil {
		t.Fatal("native identity was mutable")
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_updated_at = ?, updated_at = ?, last_activity_at = ? WHERE id = ?", testTimestamp, testTimestamp, testTimestamp, issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = 'Imported edit' WHERE id = ?", issueID); err != nil {
		t.Fatal(err)
	}
	var activity, updated, sourceUpdated string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT last_activity_at, native_updated_at, updated_at FROM issues WHERE id = ?", issueID).Scan(&activity, &updated, &sourceUpdated); err != nil {
		t.Fatal(err)
	}
	if activity != updated || activity == testTimestamp || sourceUpdated != testTimestamp {
		t.Fatalf("import edit activity = %s, native updated = %s, projection updated = %s", activity, updated, sourceUpdated)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, Config{DatabasePath: path})
	var restartedID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE id = ?", issueID).Scan(&restartedID); err != nil {
		t.Fatal(err)
	}
	if restartedID != nativeID {
		t.Fatal("restart changed native identity")
	}
	fixture := newNativeFixture(t, service, "", "coexisting-native")
	fixture.create(t, "native work")
}
