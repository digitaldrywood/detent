package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnsBackfillProgress(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	second := first.anotherChange(t)
	versions := []tracker.ChangeVersion{first.publish(t, "first", ""), second.publish(t, "second", "")}
	fixtures := []changeFixture{first, second}
	if err := first.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := first.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range changeColumnsPerTick + 2 {
		index := i % 2
		raw := fmt.Sprintf(`{"review_id":"legacy-%d","version_id":%q,"decision":"changes_requested","validator":{},"actor":{"principal_id":"validator"},"created_at":"2026-10-25T12:00:00Z"}`, i, versions[index].ID)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, fixtures[index].change.ID, versions[index].ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var value int
		if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_evidence WHERE has_validator=1`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := count(); got != 0 {
		t.Fatalf("startup populated %d historical reviews", got)
	}
	for _, f := range fixtures {
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		detail, err := f.service.readRecoveryChange(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID), f.changeWithCurrentVersion(t))
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.Reviews) == 0 || detail.Summary.Status == "reviewed" {
			t.Fatalf("unknown review authorized landing or hid refusals: %+v", detail.Summary)
		}
	}
	for tick, want := range []int{changeColumnsPerTick, changeColumnsPerTick + 2} {
		if err := first.service.populateChangeColumns(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := count(); got != want {
			t.Fatalf("tick %d converted %d rows, want %d", tick+1, got, want)
		}
	}
	changed, err := first.service.populateChangeColumn(t.Context())
	if err != nil || changed {
		t.Fatalf("completed backfill: changed=%v error=%v", changed, err)
	}
	for _, raw := range []string{`{"validator":{},"decision":"changes_requested"}`, `{"validator":"invalid"}`} {
		if _, err := first.service.database.db.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, first.change.ID, versions[0].ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.service.populateChangeColumns(t.Context()); err == nil {
		t.Fatal("invalid historical projection did not fail")
	}
	if got := count(); got != changeColumnsPerTick+3 {
		t.Fatalf("failed row rolled back earlier item's transaction: %d", got)
	}
}

func (f changeFixture) changeWithCurrentVersion(t *testing.T) tracker.ChangeRequest {
	t.Helper()
	change, err := readChange(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID), f.change.ID)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func TestChangeTypedColumnsOwnEvidenceFilters(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	second := first.anotherChange(t)
	for _, f := range []changeFixture{first, second} {
		t.Run(string(f.project.ID), func(t *testing.T) {
			old := f.publish(t, "old", "")
			current := f.publish(t, "current", old.ID)
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
			reviews := []tracker.ChangeReview{
				{ID: "stale", VersionID: old.ID, Decision: "approved", Actor: tracker.Actor{PrincipalID: "reviewer"}, CreatedAt: at},
				{ID: "implementation", VersionID: current.ID, Decision: "approved", ImplementationReview: &gate.ValidatorResult{}, CreatedAt: at},
				{ID: "validator", VersionID: current.ID, Decision: "changes_requested", Validator: &gate.ValidatorResult{}, CreatedAt: at},
				{ID: "source-audit", VersionID: current.ID, Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{}, CreatedAt: at},
				{ID: "landing-audit", VersionID: current.ID, Decision: "approved", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, CreatedAt: at},
			}
			for _, review := range reviews {
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, review.VersionID, "review", review.ID, review); err != nil {
					t.Fatal(err)
				}
			}
			check := tracker.ChangeCheck{VersionID: current.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: current.Checks[0].CheckRunID, Conclusion: "failure"}}
			if err := insertChangeEvidence(t.Context(), tx, f.change.ID, current.ID, "check", "check", check); err != nil {
				t.Fatal(err)
			}
			unrelated := check
			unrelated.CheckRunID = "unrelated"
			if err := insertChangeEvidence(t.Context(), tx, f.change.ID, current.ID, "check", "unrelated", unrelated); err != nil {
				t.Fatal(err)
			}
			for i := range 40 {
				review := tracker.ChangeReview{ID: fmt.Sprintf("comment-%d", i), VersionID: current.ID, Decision: "commented", Actor: tracker.Actor{PrincipalID: "reviewer"}, CreatedAt: at}
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, current.ID, "review", review.ID, review); err != nil {
					t.Fatal(err)
				}
			}
			for i, run := range []string{current.Checks[0].CheckRunID, "unrelated-check"} {
				check := tracker.ChangeCheck{VersionID: current.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: run, Conclusion: "failure"}}
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, current.ID, "check", strconv.Itoa(i), check); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			for _, pending := range []bool{false, true} {
				if pending {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_evidence SET has_validator=NULL,check_run_id=NULL WHERE change_id=?`, f.change.ID); err != nil {
						t.Fatal(err)
					}
				}
				detail, err := f.service.readRecoveryChange(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID), f.changeWithCurrentVersion(t))
				if err != nil {
					t.Fatal(err)
				}
				if len(detail.Checks) != 1 || detail.Checks[0].CheckRunID != check.CheckRunID || detail.Checks[0].Conclusion != "failure" {
					t.Fatalf("pending=%v lost or mismatched check: %+v", pending, detail.Checks)
				}
				ids := map[string]bool{}
				for _, review := range detail.Reviews {
					ids[review.ID] = true
				}
				for _, id := range []string{"implementation", "validator", "source-audit", "landing-audit"} {
					if !ids[id] {
						t.Fatalf("pending=%v hid %s", pending, id)
					}
				}
				if detail.Summary.Status == "reviewed" {
					t.Fatalf("pending=%v hid a refusal", pending)
				}
				if len(detail.Checks) != 1 || detail.Checks[0].CheckRunID != current.Checks[0].CheckRunID || detail.Checks[0].Conclusion != "failure" {
					t.Fatalf("pending=%v hid or misattributed a failing check: %+v", pending, detail.Checks)
				}
				prior, err := readPriorChangeApproval(t.Context(), f.service.database.reader, f.change.ID, current.ID)
				if err != nil || !prior {
					t.Fatalf("pending=%v lost prior approval: %v", pending, err)
				}
			}
			if err := f.service.populateChangeColumns(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_evidence SET claim_review_decision='commented' WHERE change_id=? AND version_id=?`, f.change.ID, old.ID); err != nil {
				t.Fatal(err)
			}
			prior, err := readPriorChangeApproval(t.Context(), f.service.database.reader, f.change.ID, current.ID)
			if err != nil || prior {
				t.Fatalf("read payload decision instead of typed decision: prior=%v error=%v", prior, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_versions SET attempt_id=? WHERE id=?`, "typed-attempt", current.ID); err != nil {
				t.Fatal(err)
			}
			matched, err := changeArtifactAttemptMatches(t.Context(), f.service.database.reader, scope, current.ID, "typed-attempt")
			if err != nil || !matched {
				t.Fatalf("typed attempt identity ignored: matched=%v error=%v", matched, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_versions SET attempt_id=NULL WHERE id=?`, current.ID); err != nil {
				t.Fatal(err)
			}
			matched, err = changeArtifactAttemptMatches(t.Context(), f.service.database.reader, scope, current.ID, "typed-attempt")
			if err != nil || matched {
				t.Fatalf("unknown attempt identity authorized receipt: matched=%v error=%v", matched, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_versions SET record_json='{}' WHERE id=?`, current.ID); err == nil {
				t.Fatal("typed backfill weakened source immutability")
			}
		})
	}
}

func TestChangeColumnProjectionValues(t *testing.T) {
	t.Parallel()
	at := "2026-10-25T12:00:00.000000000Z"
	for _, test := range []struct {
		family, raw string
		want        map[string]any
	}{
		{"change_requests", `{"landed":{"version_id":"version","head_sha":"head","merge_sha":"merge","base_ref":"develop","landed_at":"` + at + `"},"created_at":"` + at + `","updated_at":"` + at + `"}`, map[string]any{"landed_version_id": "version", "landed_head_sha": "head", "landed_merge_sha": "merge", "landed_base_ref": "develop", "landed_at": at, "created_at": at, "updated_at": at}},
		{"change_versions", `{"attempt_id":"attempt","head_sha":"head","policy_id":"policy","repository":"owner/repository","external":{"provider":"github","id":"42","url":"https://example.test/pull/42"},"created_at":"` + at + `"}`, map[string]any{"attempt_id": "attempt", "head_sha": "head", "policy_revision_id": "policy", "repository": "owner/repository", "external_provider": "github", "external_id": "42", "external_url": "https://example.test/pull/42", "created_at": at}},
		{"change_evidence", `{"decision":"approved","actor":{"principal_id":"reviewer"},"implementation_review":{},"validator":{},"security_audit":{"landing":true},"check_run_id":"check","created_at":"` + at + `","received_at":"` + at + `","escape":{"occurrence_id":"occurrence","observed_at":"` + at + `"}}`, map[string]any{"claim_review_decision": "approved", "claim_review_actor": "reviewer", "has_implementation_review": true, "has_validator": true, "has_security_audit": true, "security_audit_landing": true, "check_run_id": "check", "created_at": at, "received_at": at, "escape_occurrence_id": "occurrence", "escape_observed_at": at}},
		{"change_evidence", `{}`, map[string]any{"has_validator": false, "has_implementation_review": false, "has_security_audit": false, "security_audit_landing": false, "received_at": nil}},
		{"quality_landings", `{"landed_at":"` + at + `"}`, map[string]any{"landed_at": at}},
		{"change_landing_receipts", `{"merge_sha":"merge"}`, map[string]any{"merge_sha": "merge"}},
		{"native_attempts", `{"policy_id":"policy","runtime":{"landing":{}},"finalization":{"change_id":"final-change","publication":{"external":{"url":"https://example.test/pull/42"}}},"handoff":{"change":{"change_id":"checkpoint-change"}}}`, map[string]any{"has_landing_receipt": true, "policy_id": "policy", "finalization_change_id": "final-change", "finalization_external_url": "https://example.test/pull/42", "checkpoint_change_id": "checkpoint-change"}},
		{"collaboration_events", `{"from_state":"Done","to_state":"Rework","run":{"attempt_id":"attempt","run_id":"run","lease_id":"lease","fencing_token":"7","policy_id":"policy","sequence":"9","handoff":{"external_effect":"git_push","effect_state":"confirmed","change":{"change_id":"change","version_id":"version","head_sha":"head"}}}}`, map[string]any{"from_state": "Done", "to_state": "Rework", "push_external_effect": "git_push", "push_effect_state": "confirmed", "push_change_id": "change", "push_version_id": "version", "push_head_sha": "head", "claim_attempt_id": "attempt", "run_id": "run", "run_lease_id": tracker.LeaseID("lease"), "run_policy_id": "policy", "run_fencing_token": tracker.FencingToken(7), "run_sequence": int64(9)}},
		{"issues", `{"principal_id":"author"}`, map[string]any{"actor_principal_id": "author"}},
		{"native_comments", `{"kind":"human"}`, map[string]any{"change_actor_kind": "human"}},
	} {
		t.Run(test.family+strconv.Itoa(len(test.raw)), func(t *testing.T) {
			columns, err := changeRecordColumns(t.Context(), nil, test.family, test.raw)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for i, column := range columns.columns {
				got[column] = columns.values[i]
			}
			for column, want := range test.want {
				if !reflect.DeepEqual(got[column], want) {
					t.Errorf("%s=%#v, want %#v", column, got[column], want)
				}
			}
		})
	}
}

func TestChangeSchemaUsesTypedEvidence(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	names := []string{"change_evidence_approval_idx", "change_evidence_analytics_review", "change_evidence_analytics_escape", "change_evidence_activity", "change_requests_activity_latest", "change_requests_analytics_landed", "change_requests_policy_landing_idx", "change_requests_activity", "change_versions_attempt_idx", "quality_landings_analytics_landed", "collaboration_events_publication_push"}
	for _, name := range names {
		var definition sql.NullString
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if !definition.Valid || strings.Contains(strings.ToLower(definition.String), "json_") {
			t.Errorf("schema object %s retains stored JSON SQL: %s", name, definition.String)
		}
	}
}

func (f changeFixture) anotherChange(t *testing.T) changeFixture {
	t.Helper()
	issue := f.create(t, "other work")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "other-change"}, Title: "Other change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	return changeFixture{nativeFixture: f.nativeFixture, issue: issue, change: change, path: path + "/" + change.ID, rules: f.rules}
}

func TestChangeLandingColumnsPreserveHistoricalReads(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	second := first.anotherChange(t)
	fixtures := []changeFixture{first, second}
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	versions := make([]tracker.ChangeVersion, 2)
	for i, f := range fixtures {
		version := f.publish(t, "version", "")
		versions[i] = version
		change := f.changeWithCurrentVersion(t)
		change.Landed = &tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat(strconv.Itoa(i+1), 40), BaseRef: "develop", LandedAt: at, Actor: tracker.Actor{PrincipalID: "operator"}}
		raw, err := marshalNative(change)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET record_json=? WHERE id=?`, raw, change.ID); err != nil {
			t.Fatal(err)
		}
		if err := projectChangeColumns(t.Context(), tx, "change_requests", change.ID, raw); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			raw, err := marshalNative(change.Landed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO quality_landings(version_id,record_json) VALUES(?,?)`, version.ID, raw); err != nil {
				t.Fatal(err)
			}
			if err := projectChangeColumns(t.Context(), tx, "quality_landings", version.ID, raw); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	scope := nativeScope{organization: first.project.OrganizationID, project: first.project.ID}
	window := operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at.Add(time.Hour)}
	for _, test := range []struct {
		name, statement string
		args            []any
	}{
		{"landed version", `UPDATE change_requests SET landed_version_id='missing' WHERE id=?`, []any{first.change.ID}},
		{"landed head", `UPDATE change_requests SET landed_head_sha='different' WHERE id=?`, []any{first.change.ID}},
		{"version head", `UPDATE change_versions SET head_sha='different' WHERE id=?`, []any{versions[0].ID}},
		{"merge identity", `UPDATE change_requests SET landed_merge_sha='' WHERE id=?`, []any{first.change.ID}},
		{"landing time", `UPDATE change_requests SET landed_at=? WHERE id=?`, []any{changeColumnTime(window.To), first.change.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := first.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), test.statement, test.args...); err != nil {
				t.Fatal(err)
			}
			for _, pending := range []bool{false, true} {
				if pending {
					if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET created_at=NULL WHERE id=?`, first.change.ID); err != nil {
						t.Fatal(err)
					}
				}
				for _, read := range []func(context.Context, nativeQueryer, nativeScope, operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error){nativeAnalyticsLandings, readQualityLandings} {
					rows, err := read(t.Context(), tx, scope, window)
					want := 1
					if pending {
						want = 2
					}
					if err != nil || len(rows) != want || !pending && rows[0].ChangeID != second.change.ID {
						t.Fatalf("pending=%v landings=%+v want=%d error=%v", pending, rows, want, err)
					}
				}
			}
		})
	}

	for _, column := range []string{"landed_version_id", "landed_merge_sha", "landed_base_ref", "repository"} {
		t.Run("policy/"+column, func(t *testing.T) {
			tx, err := first.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			table, id := "change_requests", first.change.ID
			if column == "repository" {
				table, id = "change_versions", versions[0].ID
			}
			source := &policy.RepositorySource{Repository: validationRepository(versions[0].Repository), Commit: strings.Repeat("1", 40), DefaultBranch: "develop"}
			for _, pending := range []bool{false, true} {
				value := any("different")
				if pending {
					value = nil
					if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET created_at=NULL WHERE id=?`, first.change.ID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE "+table+" SET "+column+"=? WHERE id=?", value, id); err != nil {
					t.Fatal(err)
				}
				landing, found, err := readNativePolicyLanding(t.Context(), tx, string(scope.organization), string(scope.project), source)
				if err != nil || found != pending || found && landing.ChangeID != first.change.ID {
					t.Fatalf("pending=%v landing=%+v found=%v error=%v", pending, landing, found, err)
				}
			}
		})
	}

	for _, test := range []struct {
		name, commit, branch, change string
	}{
		{"matching source", strings.Repeat("1", 40), "develop", first.change.ID},
		{"different commit", strings.Repeat("3", 40), "develop", ""},
		{"different branch", strings.Repeat("1", 40), "feature", ""},
		{"later matching landing", strings.Repeat("2", 40), "develop", second.change.ID},
	} {
		t.Run("partial backfill/"+test.name, func(t *testing.T) {
			tx, err := first.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET created_at=NULL,landed_merge_sha=NULL,landed_base_ref=NULL WHERE id=?`, first.change.ID); err != nil {
				t.Fatal(err)
			}
			source := &policy.RepositorySource{Repository: validationRepository(versions[0].Repository), Commit: test.commit, DefaultBranch: test.branch}
			landing, err := readPolicyLanding(t.Context(), tx, string(scope.organization)+"/"+string(scope.project), source)
			if err != nil || landing.ChangeID != test.change || landing.Actor != "" && test.change == "" {
				t.Fatalf("landing=%+v want change=%q error=%v", landing, test.change, err)
			}
		})
	}

	for _, pending := range []bool{false, true} {
		if pending {
			for _, statement := range []string{`UPDATE change_requests SET created_at=NULL,claim_version_id=NULL,landed_version_id=NULL,landed_merge_sha=NULL,landed_base_ref=NULL,landed_at=NULL`, `UPDATE change_versions SET head_sha=NULL,repository=NULL`, `UPDATE quality_landings SET landed_at=NULL`} {
				if _, err := first.service.database.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
		}
		for i, f := range fixtures {
			source := &policy.RepositorySource{Repository: validationRepository(versions[i].Repository), Commit: strings.Repeat(strconv.Itoa(i+1), 40), DefaultBranch: "develop"}
			landing, err := readPolicyLanding(t.Context(), f.service.database.reader, string(scope.organization)+"/"+string(scope.project), source)
			if err != nil || landing.ChangeID != f.change.ID {
				t.Fatalf("pending=%v policy landing=%+v error=%v", pending, landing, err)
			}
			issue, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID))
			if err != nil || issue.ListChange == nil || issue.ListChange.HeadSHA != source.Commit || issue.ListChange.Branch != "develop" {
				t.Fatalf("pending=%v issue landing=%+v error=%v", pending, issue.ListChange, err)
			}
		}
		for _, read := range []func(context.Context, nativeQueryer, nativeScope, operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error){nativeAnalyticsLandings, readQualityLandings} {
			rows, err := read(t.Context(), first.service.database.reader, scope, window)
			if err != nil || len(rows) != 2 {
				t.Fatalf("pending=%v landings=%v error=%v", pending, rows, err)
			}
			for _, row := range rows {
				expected := map[string]string{first.change.ID: versions[0].ID, second.change.ID: versions[1].ID}
				if row.Landing.VersionID != expected[row.ChangeID] || !row.Landing.LandedAt.Equal(at) {
					t.Fatalf("pending=%v landing=%+v", pending, row)
				}
			}
		}
	}
	if err := first.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		appendTestHealthEvent(t, f.nativeFixture, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Done", ToState: "Rework"}, at.Add(3*time.Hour))
	}
	window = operatortool.AnalyticsWindow{From: at.Add(2 * time.Hour), To: at.Add(4 * time.Hour)}
	for _, pending := range []bool{true, false} {
		if pending {
			if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET from_state=NULL,to_state=NULL WHERE type='workflow.transitioned'`); err != nil {
				t.Fatal(err)
			}
		} else if err := first.service.populateChangeColumns(t.Context()); err != nil {
			t.Fatal(err)
		}
		rows, err := readQualityLandings(t.Context(), first.service.database.reader, scope, window)
		if err != nil || len(rows) != 2 {
			t.Fatalf("pending events=%v lost reopened historical landings: rows=%v error=%v", pending, rows, err)
		}
	}

}

func projectTestChangeColumns(t testing.TB, s *Service) {
	t.Helper()
	for _, family := range changeColumnFamilies {
		if family.table == "native_attempts" || family.table == "issues" || family.table == "native_comments" {
			continue
		}
		rows, err := s.database.reader.QueryContext(t.Context(), "SELECT "+family.key+","+family.payload+" FROM "+family.table+" WHERE "+family.pending+" IS NULL")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type record struct {
			id  any
			raw string
		}
		records := []record{}
		for rows.Next() {
			var r record
			if err := rows.Scan(&r.id, &r.raw); err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		tx, err := s.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			columns, err := changeRecordColumns(t.Context(), tx, family.table, r.raw)
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if family.table == "change_requests" {
				version, head, err := claimChangeIdentity(t.Context(), tx, fmt.Sprint(r.id), r.raw)
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				columns.add("claim_version_id", version)
				columns.add("claim_head_sha", head)
			}
			if err := writeChangeColumns(t.Context(), tx, family, r.id, r.raw, columns); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangeColumnsSelectIssueSideData(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		old := f.publish(t, "old", "")
		current := f.publish(t, "current", old.ID)
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		for _, test := range []struct {
			name, statement, branch, head string
			args                          []any
			pullRequest                   bool
		}{
			{name: "current version", statement: `UPDATE change_requests SET claim_version_id=? WHERE id=?`, args: []any{old.ID, f.change.ID}, head: old.HeadSHA},
			{name: "version head", statement: `UPDATE change_versions SET head_sha='typed-head' WHERE id=?`, args: []any{current.ID}, head: "typed-head"},
			{name: "current landing", statement: `UPDATE change_requests SET landed_version_id=?,landed_merge_sha='typed-merge',landed_base_ref='typed-base' WHERE id=?`, args: []any{current.ID, f.change.ID}, branch: "typed-base", head: "typed-merge"},
			{name: "old landing", statement: `UPDATE change_requests SET landed_version_id=?,landed_merge_sha='typed-merge',landed_base_ref='typed-base' WHERE id=?`, args: []any{old.ID, f.change.ID}, head: current.HeadSHA},
			{name: "external reference", statement: `UPDATE change_versions SET external_provider='github',external_id='42',external_url='https://example.test/pull/42' WHERE id=?`, args: []any{current.ID}, head: current.HeadSHA, pullRequest: true},
			{name: "external provider", statement: `UPDATE change_versions SET external_provider='other',external_id='42',external_url='https://example.test/pull/42' WHERE id=?`, args: []any{current.ID}, head: current.HeadSHA},
			{name: "empty external URL", statement: `UPDATE change_versions SET external_provider='github',external_id='42',external_url='' WHERE id=?`, args: []any{current.ID}, head: current.HeadSHA},
		} {
			t.Run(test.name+"/"+f.change.ID, func(t *testing.T) {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), test.statement, test.args...); err != nil {
					t.Fatal(err)
				}
				for _, pending := range []bool{false, true} {
					if pending {
						if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET created_at=NULL WHERE id=?`, f.change.ID); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(t.Context(), `UPDATE change_versions SET head_sha=NULL WHERE id=?`, current.ID); err != nil {
							t.Fatal(err)
						}
					}
					issue, _, err := readNativeIssueProjection(t.Context(), tx, scope, string(f.issue.WorkItemID), false)
					if err != nil {
						t.Fatal(err)
					}
					head, branch, pull := test.head, test.branch, test.pullRequest
					if pending && test.name == "version head" {
						head = current.HeadSHA
					}
					if issue.ListChange == nil || issue.ListChange.HeadSHA != head || issue.ListChange.Branch != branch {
						t.Fatalf("pending=%v issue Change=%+v want branch=%s head=%s", pending, issue.ListChange, branch, head)
					}
					if (issue.PullRequest != nil) != pull || pull && (issue.PullRequest.Number != 42 || issue.PullRequest.URL != "https://example.test/pull/42") {
						t.Fatalf("pending=%v issue PR=%+v", pending, issue.PullRequest)
					}
				}
			})
		}
	}
}

func TestChangeColumnsSelectRecoveryComments(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	second := first.anotherChange(t)
	for _, f := range []changeFixture{first, second} {
		t.Run(string(f.issue.WorkItemID), func(t *testing.T) {
			ids := []string{}
			for i, kinds := range [][2]string{{"runner", ""}, {"human", ""}, {"runner", "human"}, {"runner", ""}} {
				id := newNativeID("cmt")
				ids = append(ids, id)
				actor, err := marshalNative(tracker.Actor{Kind: kinds[0], PrincipalID: "author"})
				if err != nil {
					t.Fatal(err)
				}
				editor, err := marshalNative(tracker.Actor{Kind: kinds[1], PrincipalID: "editor"})
				if err != nil {
					t.Fatal(err)
				}
				at := formatHubTime(f.service.config.now())
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,edited_by_json,created_at,updated_at,change_actor_kind,change_edited_by_kind) VALUES(?,?,?,?,1,?,'comment',?,?,?,?,?,?)`, id, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, i+1, actor, editor, at, at, kinds[0], kinds[1]); err != nil {
					t.Fatal(err)
				}
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			for _, state := range []string{"typed", "typed author changed", "unconverted"} {
				want := []string{ids[1], ids[2], ids[3]}
				if state == "typed author changed" {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_comments SET change_actor_kind='runner' WHERE id=?`, ids[1]); err != nil {
						t.Fatal(err)
					}
					want = []string{ids[2], ids[3]}
				}
				if state == "unconverted" {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_comments SET change_actor_kind=NULL,change_edited_by_kind=NULL WHERE work_item_id=?`, f.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				for _, bounded := range []bool{false, true} {
					order := " ORDER BY sequence"
					expected := append([]string{}, want...)
					if bounded {
						order = " ORDER BY sequence DESC LIMIT 33"
						for left, right := 0, len(expected)-1; left < right; left, right = left+1, right-1 {
							expected[left], expected[right] = expected[right], expected[left]
						}
					}
					got, err := readRecoveryCommentIDs(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID), order, bounded)
					if err != nil || !reflect.DeepEqual(got, expected) {
						t.Fatalf("%s bounded=%v: comments=%v want=%v error=%v", state, bounded, got, expected, err)
					}
				}
			}
		})
	}
}

func TestChangeColumnsSelectEscapeOccurrence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		old := f.publish(t, "old", "")
		version := f.publish(t, "version", old.ID)
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var eventIDs [3]string
		for i, data := range []tracker.CollaborationData{{FromState: "Done", ToState: "Todo"}, {FromState: "Merging", ToState: "Rework"}, {FromState: "Todo", ToState: "In Progress"}} {
			if err := appendNativeHistory(t.Context(), tx, scope, string(f.issue.WorkItemID), "workflow.transitioned", data, at.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
			var id string
			if err := tx.QueryRowContext(t.Context(), `SELECT id FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1`, f.issue.WorkItemID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			eventIDs[i] = id
		}
		for i, occurrence := range []string{eventIDs[0], "unrelated"} {
			discussion := tracker.ChangeDiscussion{ID: fmt.Sprintf("discussion-%d", i), VersionID: version.ID, CreatedAt: at, Escape: &tracker.QualityEscape{OccurrenceID: occurrence, ObservedAt: at}}
			if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "discussion", discussion.ID, discussion); err != nil {
				t.Fatal(err)
			}
		}
		for _, discussion := range []tracker.ChangeDiscussion{
			{ID: "ordinary", VersionID: version.ID},
			{ID: "other version", VersionID: old.ID, Escape: &tracker.QualityEscape{OccurrenceID: eventIDs[0]}},
		} {
			if err := insertChangeEvidence(t.Context(), tx, f.change.ID, discussion.VersionID, "discussion", discussion.ID, discussion); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE change_evidence SET escape_occurrence_id='changed' WHERE change_id=? AND version_id=? AND escape_occurrence_id=?`, f.change.ID, version.ID, eventIDs[0]); err != nil {
			t.Fatal(err)
		}
		if raw, err := readEscapeOccurrence(t.Context(), tx, f.change.ID, version.ID, eventIDs[0]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("typed occurrence ignored: raw=%s error=%v", raw, err)
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE change_evidence SET escape_occurrence_id=? WHERE change_id=? AND version_id=? AND escape_occurrence_id='changed'`, eventIDs[0], f.change.ID, version.ID); err != nil {
			t.Fatal(err)
		}
		for _, pending := range []bool{false, true} {
			if pending {
				for _, statement := range []string{`UPDATE collaboration_events SET from_state=NULL,to_state=NULL WHERE work_item_id=?`, `UPDATE change_evidence SET escape_occurrence_id=NULL WHERE change_id=?`} {
					key := string(f.issue.WorkItemID)
					if strings.Contains(statement, "change_evidence") {
						key = f.change.ID
					}
					if _, err := tx.ExecContext(t.Context(), statement, key); err != nil {
						t.Fatal(err)
					}
				}
			}
			for i, event := range eventIDs {
				valid, err := qualityReopenOccurrence(t.Context(), tx, scope, string(f.issue.WorkItemID), tracker.QualityEscape{OccurrenceID: event, ObservedAt: at.Add(time.Duration(i) * time.Second)})
				if err != nil || valid != (i == 0) {
					t.Fatalf("pending=%v event=%d reopened=%v error=%v", pending, i, valid, err)
				}
			}
			exit, found, err := readLatestLandingExit(t.Context(), tx, scope, string(f.issue.WorkItemID))
			if err != nil || !found || exit.ID != eventIDs[1] {
				t.Fatalf("pending=%v landing exit=%+v found=%v error=%v", pending, exit, found, err)
			}
			raw, err := readEscapeOccurrence(t.Context(), tx, f.change.ID, version.ID, eventIDs[0])
			var discussion tracker.ChangeDiscussion
			if err != nil || decodeStorageJSON(t.Context(), []byte(raw), &discussion) != nil || discussion.ID != "discussion-0" {
				t.Fatalf("pending=%v escape=%+v error=%v", pending, discussion, err)
			}
			_, err = readEscapeOccurrence(t.Context(), tx, f.change.ID, version.ID, "missing")
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("pending=%v unrelated occurrence matched: %v", pending, err)
			}
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangeColumnsSelectAttemptEvidence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		old := f.publish(t, "old", "")
		current := f.publish(t, "current", old.ID)
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
		runner.enroll(t)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		at := f.service.config.now()
		attempts := []string{}
		for i, version := range []tracker.ChangeVersion{old, current} {
			receipt := tracker.NativeLandingReceipt{VersionID: version.ID, MergeSHA: strings.Repeat(strconv.Itoa(i+1), 40), Landed: true}
			run := tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: int64(100 + i), Landing: &receipt}}
			attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", run, at, -1)
			attempts = append(attempts, attempt)
			if err := updateAttemptHotProjection(t.Context(), tx, attempt, "succeeded", run); err != nil {
				t.Fatal(err)
			}
			if err := persistAttemptLanding(t.Context(), tx, attempt, receipt); err != nil {
				t.Fatal(err)
			}
		}
		seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", tracker.NativeRunData{}, at, -1)
		for _, test := range []struct {
			name  string
			value any
			want  string
		}{
			{"present", true, attempts[1]},
			{"absent", false, attempts[0]},
			{"unknown", nil, attempts[1]},
		} {
			t.Run(test.name+"/"+f.change.ID, func(t *testing.T) {
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET has_landing_receipt=? WHERE id=?`, test.value, attempts[1]); err != nil {
					t.Fatal(err)
				}
				got, err := latestRecoveryLandingAttempt(t.Context(), tx, scope, string(f.issue.WorkItemID))
				if err != nil || got != test.want {
					t.Fatalf("landing attempt=%s want=%s error=%v", got, test.want, err)
				}
			})
		}
		for _, test := range []struct {
			name  string
			merge any
			want  string
		}{
			{"typed priority", strings.Repeat("1", 40), old.ID},
			{"typed mismatch", "different", current.ID},
			{"unknown matching receipt ranks first", nil, old.ID},
		} {
			t.Run(test.name+"/"+f.change.ID, func(t *testing.T) {
				if _, err := tx.ExecContext(t.Context(), `UPDATE change_landing_receipts SET merge_sha=? WHERE attempt_id=?`, test.merge, attempts[0]); err != nil {
					t.Fatal(err)
				}
				rows, partial, err := readValidationCandidates(t.Context(), tx, scope, string(f.issue.WorkItemID), "", "", gate.ScheduledEvidence{Repository: validationRepository(current.Repository), HeadSHA: strings.Repeat("1", 40)})
				if err != nil || partial || len(rows) != 2 || rows[0].VersionID != test.want {
					t.Fatalf("candidates=%+v partial=%v error=%v", rows, partial, err)
				}
				for _, row := range rows {
					want := int64(100)
					if row.VersionID == current.ID {
						want++
					}
					if row.LocalAttemptID != want {
						t.Fatalf("local attempt=%d want=%d", row.LocalAttemptID, want)
					}
				}
			})
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangeAnalyticsFallbackServesLegacyHistory(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"landing", "discussion", "event", "evidence"} {
		t.Run(family, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			other := f.anotherChange(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			at := f.service.config.now()
			versions := []tracker.ChangeVersion{f.publish(t, "first", ""), other.publish(t, "second", "")}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			population := maxAnalyticsPopulation
			if family == "evidence" {
				population /= 2
			}
			for index, fixture := range []changeFixture{f, other} {
				for i := range population {
					id := fmt.Sprintf("history-%s-%04d", fixture.issue.WorkItemID, i)
					switch family {
					case "landing":
						if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,'{}')`, id, f.project.OrganizationID, f.project.ID, fixture.issue.WorkItemID); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,'{}')`, "version-"+id, id); err != nil {
							t.Fatal(err)
						}
					case "discussion":
						if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'discussion','{}')`, fixture.change.ID, versions[index].ID); err != nil {
							t.Fatal(err)
						}
					case "event":
						if err := appendNativeHistory(t.Context(), tx, scope, string(fixture.issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, at.Add(-time.Minute)); err != nil {
							t.Fatal(err)
						}
					case "evidence":
						if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review','{"decision":"commented"}')`, fixture.change.ID, versions[index].ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if family == "event" {
				if _, err := tx.ExecContext(t.Context(), `UPDATE collaboration_events SET from_state=NULL WHERE type='workflow.transitioned'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			window := operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at}
			if family == "evidence" {
				inputs, err := loadAnalyticsQualityInputs(t.Context(), f.service.database.reader, scope, window)
				if err != nil || inputs.partial || len(inputs.discussions) != 0 {
					t.Fatalf("boundary evidence read failed: inputs=%+v error=%v", inputs, err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review','{"decision":"commented"}')`, f.change.ID, versions[0].ID); err != nil {
					t.Fatal(err)
				}
				_, err = loadAnalyticsQualityInputs(t.Context(), f.service.database.reader, scope, window)
				if err != nil {
					t.Fatalf("legacy evidence unavailable: %v", err)
				}
				return
			}
			for _, quality := range []bool{false, true} {
				if !quality && family != "landing" {
					continue
				}
				_, err := readUnconvertedAnalyticsLandings(t.Context(), f.service.database.reader, scope, window, quality)
				if err != nil {
					t.Fatalf("quality=%v legacy %s unavailable: %v", quality, family, err)
				}
			}
		})
	}
}

func TestChangeColumnBackfillUsesInlineAndRetriesArchive(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "version", "")
	if err := f.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.service.config.Content = func(context.Context, entryauthority.ContentRequest) ([]byte, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	data := []byte(`{"decision":"approved","validator":{},"created_at":"2026-10-25T12:00:00Z"}`)
	payload, err := json.Marshal(storageJSONPayload{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_evidence", Row: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	reference := storageJSONReference{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_evidence", Row: 1, Reference: ref}
	envelope, err := json.Marshal(map[string]any{"_storage_archive": reference})
	if err != nil {
		t.Fatal(err)
	}
	inline, err := compactStorageJSON(data, reference)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{envelope, inline} {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, f.change.ID, version.ID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.populateChangeColumns(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("archive failure=%v", err)
	}
	var pending, converted int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE has_validator IS NULL),count(*) FILTER(WHERE has_validator=1) FROM change_evidence WHERE change_id=?`, f.change.ID).Scan(&pending, &converted); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || converted != 1 || calls != 1 {
		t.Fatalf("pending=%d converted=%d archive reads=%d", pending, converted, calls)
	}
	objects := &changeObjectFixture{}
	_, compressed, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.call(t.Context(), entryauthority.ContentRequest{Organization: string(f.project.OrganizationID), Reference: ref, Data: compressed, Write: true}); err != nil {
		t.Fatal(err)
	}
	f.service.config.Content = objects.call
	if err := f.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_evidence WHERE change_id=? AND has_validator=1`, f.change.ID).Scan(&converted); err != nil || converted != 2 {
		t.Fatalf("retry converted=%d error=%v", converted, err)
	}
	change := f.changeWithCurrentVersion(t)
	changeRaw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = json.Marshal(storageJSONPayload{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_requests", Row: 2, Data: changeRaw})
	if err != nil {
		t.Fatal(err)
	}
	ref, compressed, err = attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.call(t.Context(), entryauthority.ContentRequest{Organization: string(f.project.OrganizationID), Reference: ref, Data: compressed, Write: true}); err != nil {
		t.Fatal(err)
	}
	envelope, err = json.Marshal(map[string]any{"_storage_archive": storageJSONReference{Organization: string(f.project.OrganizationID), Project: string(f.project.ID), Family: "change_requests", Row: 2, Reference: ref}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_requests SET record_json=?,created_at=NULL WHERE id=?`, string(envelope), f.change.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	var claimVersion, claimHead string
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT claim_version_id,claim_head_sha FROM change_requests WHERE id=?`, f.change.ID).Scan(&claimVersion, &claimHead); err != nil {
		t.Fatal(err)
	}
	if claimVersion != version.ID || claimHead != version.HeadSHA {
		t.Fatalf("archive-only identity=%s/%s want=%s/%s", claimVersion, claimHead, version.ID, version.HeadSHA)
	}

}
