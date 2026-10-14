package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runtimeReadQuery struct {
	nativeQueryer
	statements []string
	arguments  [][]any
}

func (q *runtimeReadQuery) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.statements = append(q.statements, statement)
	q.arguments = append(q.arguments, args)
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (q *runtimeReadQuery) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	q.statements = append(q.statements, statement)
	q.arguments = append(q.arguments, args)
	return q.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func TestNativeRuntimeReadHistoryCost(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "current", "")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	db := f.service.database.db
	previous, count := 0, 0
	for _, history := range []int{1, 64, 256} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			for i := previous; i < history; i++ {
				old := version
				old.ID, old.Number = newNativeID("version"), int64(i+1000)
				raw, err := marshalNative(old)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO change_versions (id, change_id, number, record_json) VALUES (?, ?, ?, ?)", old.ID, old.ChangeID, old.Number, raw); err != nil {
					t.Fatal(err)
				}
				for kind, record := range map[string]any{
					"review":     tracker.ChangeReview{ID: newNativeID("review"), VersionID: old.ID, Decision: "approved", Body: strings.Repeat("historical review ", 512), Actor: tracker.Actor{PrincipalID: "old-reviewer"}},
					"check":      tracker.ChangeCheck{VersionID: old.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: newNativeID("check"), Conclusion: "failure"}},
					"discussion": tracker.ChangeDiscussion{ID: newNativeID("comment"), VersionID: old.ID, Body: strings.Repeat("historical discussion ", 512)},
				} {
					raw, err := marshalNative(record)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, ?, ?)", old.ChangeID, old.ID, kind, raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			previous = history
			q := &runtimeReadQuery{nativeQueryer: db}
			evidence, err := readNativeRuntime(t.Context(), q, scope, string(f.issue.WorkItemID), "", f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			full := f.detail(t)
			want := full.Summary
			want.Messages = nil
			if !reflect.DeepEqual(evidence.Change.Summary, want) || evidence.Change.Change.CurrentVersion != version.ID || len(full.Versions) != history+1 || len(full.Discussion) != history || evidence.Change.Versions != nil || evidence.Change.Reviews != nil || evidence.Change.Checks != nil || evidence.Change.Discussion != nil {
				t.Fatalf("runtime lost current summary or loaded history: %#v", evidence.Change)
			}
			if count == 0 {
				count = len(q.statements)
			}
			if len(q.statements) != count {
				t.Fatalf("runtime query count grew with history: %d -> %d", count, len(q.statements))
			}
			for i, statement := range q.statements {
				if !strings.Contains(statement, "FROM change_evidence") && !strings.Contains(statement, "FROM change_versions") {
					continue
				}
				rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, q.arguments[i]...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var plans []string
				for rows.Next() {
					var id, parent, unused int
					var plan string
					if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
						t.Fatal(err)
					}
					plans = append(plans, plan)
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
				index := "change_evidence_current_idx"
				if strings.Contains(statement, "FROM change_versions") {
					index = "sqlite_autoindex_change_versions_1 (id=?)"
				} else if strings.Contains(statement, "SELECT EXISTS") {
					index = "change_evidence_approval_idx"
				}
				if !strings.Contains(strings.Join(plans, " "), index) {
					t.Fatalf("evidence query scans history: %v", plans)
				}
			}
			t.Logf("%d historical versions: %d runtime queries; indexed current evidence", history, len(q.statements))
		})
	}
}

func TestNativeRuntimeReadSharedCapacityCost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "runtime-capacity")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "selected runtime")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	report := capacityReport(now)
	report.MaxConcurrent = 8
	publishCapacity(t, f, first, report)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", first.redemption.Credential, providerClaim(first, issue, "reserved"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	previous := 1
	for _, total := range []int{1, 8, 100, 101} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			for i := previous; i < total; i++ {
				sharedRunner(t, first)
			}
			previous = total
			raw, err := marshalNative([]any{report})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json = ? WHERE organization_id = ?", raw, scope.organization); err != nil {
				t.Fatal(err)
			}
			q := &runtimeReadQuery{nativeQueryer: f.service.database.db}
			runners, truncated, err := readRuntimeRunners(t.Context(), q, scope, now)
			if err != nil || len(runners) != min(total, 100) || truncated != (total > 100) || len(q.statements) != 4 {
				t.Fatalf("capacity projection runners=%d truncated=%v queries=%d error=%v", len(runners), truncated, len(q.statements), err)
			}
			for i, r := range runners {
				wantUsed := 0
				if r.RunnerID == first.binding.RunnerID {
					wantUsed = 1
				}
				if r.HostUsed != 1 || r.Used != wantUsed || len(r.ProviderCapacity) != 1 || r.ProviderCapacity[0].Used != 1 || r.ProviderCapacity[0].MaxConcurrent != 8 {
					t.Fatalf("shared occupancy lost: %#v", r)
				}
				if i == 0 || i == len(runners)-1 || total <= 8 {
					full, err := readRunner(t.Context(), f.service.database.db, scope.organization, r.RunnerID, now)
					if err != nil || r.Health != full.Health || !reflect.DeepEqual(r.ProviderCapacity, full.ProviderCapacity) || !reflect.DeepEqual(r.Exclusions(scope.project, policy.Requirements{}, false), full.Exclusions(scope.project, policy.Requirements{}, false)) {
						t.Fatalf("projection differs from routing owner: %#v, %#v, %v", r, full, err)
					}
				}
			}
			evidence, err := readNativeRuntime(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), "", now)
			if err != nil || slices.Contains(evidence.Unavailable, "capacity_projection_truncated") != truncated || evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID {
				t.Fatalf("runtime truncation or fenced lease lost: unavailable=%v lease=%#v error=%v", evidence.Unavailable, evidence.CurrentLease, err)
			}
			if total == 101 {
				currentScope := scope
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM runner_identities WHERE organization_id=? ORDER BY id DESC LIMIT 1", scope.organization).Scan(&currentScope.credential.Runner.RunnerID); err != nil {
					t.Fatal(err)
				}
				currentQuery := &runtimeReadQuery{nativeQueryer: f.service.database.db}
				current, truncated, err := readRuntimeRunners(t.Context(), currentQuery, currentScope, now)
				if err != nil || !truncated || len(current) != 100 || current[0].RunnerID != currentScope.credential.Runner.RunnerID || len(currentQuery.statements) != 4 {
					t.Fatalf("bounded runtime projection dropped current registered runner: runners=%d truncated=%v queries=%d error=%v", len(current), truncated, len(currentQuery.statements), err)
				}
			}
			t.Logf("%d runners sharing a host/provider: %d capacity queries", total, len(q.statements))
		})
	}
	foreignOrganization := tracker.OrganizationID(newNativeID("org"))
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)", foreignOrganization, "Foreign", formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, foreignOrganization, "runtime-foreign")
	runners, _, err := readRuntimeRunners(t.Context(), f.service.database.db, nativeScope{organization: foreign.project.OrganizationID, project: foreign.project.ID}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("cross-organization capacity leaked: %#v %v", runners, err)
	}
	other := newNativeFixture(t, f.service, scope.organization, "runtime-ungranted")
	runners, _, err = readRuntimeRunners(t.Context(), f.service.database.db, nativeScope{organization: scope.organization, project: other.project.ID}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("ungranted project capacity leaked: %#v %v", runners, err)
	}
	q := &runtimeReadQuery{nativeQueryer: f.service.database.db}
	runners, _, err = readRuntimeRunners(t.Context(), q, scope, now.Add(91*time.Second))
	if err != nil || len(q.statements) != 4 {
		t.Fatal(err)
	}
	for _, r := range runners {
		if r.HostUsed != 0 || r.Used != 0 || r.ProviderCapacity[0].Used != 0 {
			t.Fatalf("expired reservation remains occupied: %#v", r)
		}
	}
	evidence, err := readNativeRuntime(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), "", now.Add(91*time.Second))
	if err != nil || evidence.Scheduling.Outcome != "unknown" || !slices.Contains(evidence.Unavailable, "capacity_projection_truncated") {
		t.Fatalf("truncated exclusions became a complete scheduling decision: %#v %v", evidence.Scheduling, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id = (SELECT token_id FROM runner_identities WHERE id = ?) AND project_id = ?", first.binding.RunnerID, scope.project); err != nil {
		t.Fatal(err)
	}
	runners, truncated, err := readRuntimeRunners(t.Context(), f.service.database.db, scope, now)
	if err != nil || truncated || len(runners) != 100 {
		t.Fatalf("project grant removal did not restrict the page: runners=%d truncated=%v error=%v", len(runners), truncated, err)
	}
	for _, r := range runners {
		if r.RunnerID == first.binding.RunnerID || r.HostUsed != 1 || r.Used != 0 || r.ProviderCapacity[0].Used != 1 {
			t.Fatalf("occupancy outside the granted runner page was lost: %#v", r)
		}
	}
}
