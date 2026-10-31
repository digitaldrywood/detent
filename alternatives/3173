package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func TestAttemptProjectionWriters(t *testing.T) {
	t.Parallel()
	backend, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "store.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	first := `{"pr_number":7,"pr_head_sha":"head","pr_base_sha":"base","run_mode":"local","forge_wait":{"host":"forge.example","error_class":"credential"},"forge_write_completed_host":"other.example"}`
	second := `{"pr_number":8,"pr_head_sha":"next-head","pr_base_sha":"next-base","run_mode":"remote","historical_completion_fence":{"excluded_from_worker_outcomes":true}}`
	for _, project := range []string{"one", "two"} {
		id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: project, IssueID: "issue", WorkerType: "codex", StartedAt: now, WorkerMetadataJSON: first, MetricsJSON: `{"turns":0}`})
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name                       string
			write                      func() error
			number                     int64
			head, base, mode           string
			excluded, wait, turns      int64
			host, waitError, completed string
		}{
			{"start", func() error { return nil }, 7, "head", "base", "local", 0, 1, 0, "forge.example", "credential", "other.example"},
			{"heartbeat", func() error {
				return backend.RecordWorkAttemptHeartbeat(t.Context(), WorkAttemptHeartbeat{AttemptID: id, HeartbeatAt: now, WorkerMetadataJSON: second, MetricsJSON: `{"turns":3}`})
			}, 8, "next-head", "next-base", "remote", 1, 0, 3, "", "", ""},
			{"completion", func() error {
				return backend.CompleteWorkAttempt(t.Context(), WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Second), TerminalState: WorkAttemptTerminalCapacity, ErrorClass: "forge_unavailable", WorkerMetadataJSON: first, MetricsJSON: `{"turns":0}`})
			}, 7, "head", "base", "local", 0, 1, 0, "forge.example", "credential", "other.example"},
			{"terminal wait", func() error {
				return backend.UpdateTerminalWorkAttemptWait(t.Context(), WorkAttemptTerminalWaitUpdate{AttemptID: id, ExpectedErrorClass: "forge_unavailable", TerminalState: WorkAttemptTerminalCapacity, ErrorClass: "forge_unavailable", WorkerMetadataJSON: second})
			}, 8, "next-head", "next-base", "remote", 1, 0, 0, "", "", ""},
			{"operator stop", func() error {
				if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET terminal_state='operator_stopped' WHERE id=?`, id); err != nil {
					return err
				}
				return backend.UpdateOperatorStop(t.Context(), OperatorStopUpdate{AttemptID: id, WorkerMetadataJSON: first})
			}, 7, "head", "base", "local", 0, 1, 0, "forge.example", "credential", "other.example"},
		}
		for _, test := range tests {
			t.Run(project+"/"+test.name, func(t *testing.T) {
				if err := test.write(); err != nil {
					t.Fatal(err)
				}
				var number, excluded, wait, turns, ready int64
				var head, base, mode string
				var host, waitError, completed sql.NullString
				if err := backend.db.QueryRowContext(t.Context(), `SELECT metadata_pr_number,metadata_pr_head_sha,metadata_pr_base_sha,metadata_run_mode,outcome_excluded,forge_wait_present,forge_wait_host,forge_wait_error_class,forge_write_completed_host,metrics_turns,projection_ready FROM work_attempts WHERE id=?`, id).Scan(&number, &head, &base, &mode, &excluded, &wait, &host, &waitError, &completed, &turns, &ready); err != nil {
					t.Fatal(err)
				}
				got := []any{number, head, base, mode, excluded, wait, turns, host.String, waitError.String, completed.String, ready}
				want := []any{test.number, test.head, test.base, test.mode, test.excluded, test.wait, test.turns, test.host, test.waitError, test.completed, int64(1)}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("projection=%v want %v", got, want)
				}
			})
		}
		if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET worker_metadata_json='{}' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		ids, err := backend.ListLocalAdmittedIssueIDs(t.Context(), project)
		if err != nil || !reflect.DeepEqual(ids, []string{"issue"}) {
			t.Fatalf("typed run mode read=%v err=%v", ids, err)
		}
	}
	for _, test := range []struct {
		name, metadata string
		excluded       bool
	}{
		{"missing", `{}`, false},
		{"false", `{"historical_completion_fence":{"excluded_from_worker_outcomes":false}}`, false},
		{"null", `{"historical_completion_fence":{"excluded_from_worker_outcomes":null}}`, false},
		{"zero", `{"historical_completion_fence":{"excluded_from_worker_outcomes":0}}`, false},
		{"real zero", `{"historical_completion_fence":{"excluded_from_worker_outcomes":0.0}}`, false},
		{"true", `{"historical_completion_fence":{"excluded_from_worker_outcomes":true}}`, true},
		{"nonzero", `{"historical_completion_fence":{"excluded_from_worker_outcomes":2}}`, true},
		{"text zero", `{"historical_completion_fence":{"excluded_from_worker_outcomes":"0"}}`, true},
		{"empty text", `{"historical_completion_fence":{"excluded_from_worker_outcomes":""}}`, true},
	} {
		t.Run("outcome/"+test.name, func(t *testing.T) {
			id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: "outcomes", IssueID: test.name, WorkerType: "codex", StartedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.CompleteWorkAttempt(t.Context(), WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Second), TerminalState: WorkAttemptTerminalFailure, WorkerMetadataJSON: test.metadata}); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET worker_metadata_json='{"historical_completion_fence":{"excluded_from_worker_outcomes":true}}' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			attempts, err := backend.ListRecentTerminalWorkAttempts(t.Context(), WorkAttemptHistoryQuery{ProjectID: "outcomes", IssueID: test.name})
			if err != nil || (len(attempts) == 0) != test.excluded {
				t.Fatalf("outcomes=%v excluded=%t err=%v", attempts, test.excluded, err)
			}
		})
	}

}

func TestTypedProjectionBackfillBoundAndCommit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	backend, err := openSQLite(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var first, second int64
	for i := range storeProjectionBatch + 3 {
		id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: fmt.Sprintf("project-%d", i%2), IssueID: fmt.Sprintf("issue-%d", i), WorkerType: "codex", StartedAt: now, WorkerMetadataJSON: `{"run_mode":"local"}`, MetricsJSON: `{"turns":0}`})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
		if i == 1 {
			second = id
		}
	}
	if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET projection_ready=NULL,metadata_run_mode=NULL,outcome_excluded=NULL,metrics_turns=NULL`); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project-0", "project-1"} {
		ids, err := backend.ListLocalAdmittedIssueIDs(t.Context(), project)
		want := (storeProjectionBatch + 3) / 2
		if project == "project-0" {
			want++
		}
		if err != nil || len(ids) != want {
			t.Fatalf("legacy evaluation %s=%v err=%v", project, ids, err)
		}
	}
	var authorized int
	if err := backend.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM work_attempts WHERE outcome_excluded=0 OR metrics_turns=0`).Scan(&authorized); err != nil {
		t.Fatal(err)
	}
	if authorized != 0 {
		t.Fatalf("unknown rows authorized=%d", authorized)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_projection BEFORE UPDATE OF projection_ready ON work_attempts WHEN NEW.id=%d BEGIN SELECT RAISE(ABORT,'fixture rejection'); END`, second)
	if _, err := backend.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	count, err := backend.BackfillTypedProjections(t.Context())
	if err == nil || count != 1 {
		t.Fatalf("partial tick=%d err=%v", count, err)
	}
	var ready sql.NullInt64
	if err := backend.db.QueryRowContext(t.Context(), `SELECT projection_ready FROM work_attempts WHERE id=?`, first).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready.Valid || ready.Int64 != 1 {
		t.Fatal("first item did not commit before second item failed")
	}
	if _, err := backend.db.ExecContext(t.Context(), `DROP TRIGGER reject_projection`); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err = openSQLite(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for tick, want := range []int{storeProjectionBatch, 2, 0} {
		count, err := backend.BackfillTypedProjections(t.Context())
		if err != nil || count != want {
			t.Fatalf("tick %d count=%d want=%d err=%v", tick, count, want, err)
		}
	}
	var remaining int
	if err := backend.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM work_attempts WHERE projection_ready IS NULL OR metadata_run_mode!='local' OR outcome_excluded!=0 OR metrics_turns!=0`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("unfinished projections=%d", remaining)
	}
	reader, err := backend.projectionReader(t.Context(), "work_attempts", "")
	if err != nil || reader != backend.db {
		t.Fatalf("completed backfill still uses fallback: %T %v", reader, err)
	}
}

func TestAttemptProjectionFallbackSourceFence(t *testing.T) {
	t.Parallel()
	backend, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "store.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	for _, project := range []string{"one", "two"} {
		id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: project, IssueID: "issue", WorkerType: "codex", StartedAt: time.Now(), WorkerMetadataJSON: `{"run_mode":"local"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET projection_ready=NULL,metadata_run_mode=NULL WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		reader, err := backend.projectionReader(t.Context(), "work_attempts", project)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.db.ExecContext(t.Context(), `UPDATE work_attempts SET worker_metadata_json='{}' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		ids, err := sqlc.New(reader).ListLocalAdmittedIssueIDs(t.Context(), sqlc.ListLocalAdmittedIssueIDsParams{ProjectID: project, LiveForeignGenerations: []int64(nil)})
		if err != nil || len(ids) != 0 {
			t.Fatalf("stale source authorized=%v err=%v", ids, err)
		}
	}
}

func TestWorkflowMetadataReplacementUpdatesTypedIdentity(t *testing.T) {
	t.Parallel()
	backend, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "store.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	for _, project := range []string{"one", "two"} {
		id, err := backend.RecordWorkflowPhaseEvent(t.Context(), WorkflowPhaseEvent{ProjectID: project, IssueID: "issue", PhaseType: WorkflowPhaseTypeLane, PhaseName: "Done", StartedAt: time.Now(), MetadataJSON: `{"retry_identity":"first","input_hash":"first-hash","correlation_id":"first-correlation","terminal_outcome":"artifact"}`})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name, metadata string
			want           []sql.NullString
		}{
			{"recorded", "", []sql.NullString{{String: "first", Valid: true}, {String: "first-hash", Valid: true}, {String: "first-correlation", Valid: true}, {String: "artifact", Valid: true}}},
			{"corrected", `{"retry_identity":"second","input_hash":"second-hash","correlation_id":"second-correlation","terminal_outcome":"operational"}`, []sql.NullString{{String: "second", Valid: true}, {String: "second-hash", Valid: true}, {String: "second-correlation", Valid: true}, {String: "operational", Valid: true}}},
			{"missing", `{}`, make([]sql.NullString, 4)},
			{"invalid types", `{"retry_identity":1,"input_hash":false,"correlation_id":{},"terminal_outcome":[]}`, make([]sql.NullString, 4)},
		} {
			t.Run(project+"/"+test.name, func(t *testing.T) {
				if test.metadata != "" {
					if err := backend.UpdateWorkflowPhaseEventMetadata(t.Context(), id, test.metadata); err != nil {
						t.Fatal(err)
					}
				}
				got := make([]sql.NullString, 4)
				var ready int
				if err := backend.db.QueryRowContext(t.Context(), `SELECT retry_identity,input_hash,correlation_id,terminal_outcome,projection_ready FROM workflow_phase_events WHERE id=?`, id).Scan(&got[0], &got[1], &got[2], &got[3], &ready); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, test.want) || ready != 1 {
					t.Fatalf("identity=%v ready=%d want=%v", got, ready, test.want)
				}
			})
		}
	}
}
