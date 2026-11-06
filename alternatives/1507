package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLegacyReadsServeOverflow(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"attempts", "side data", "policy", "escape", "comments", "landing attempt", "reviews"} {
		t.Run(family, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			version := f.publish(t, "version", "")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			machine := ""
			if family == "landing attempt" {
				runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
				runner.enroll(t)
				machine = string(runner.binding.MachineID)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if family == "reviews" {
				review := tracker.ChangeReview{VersionID: version.ID, ImplementationReview: &gate.ValidatorResult{}}
				if err := seedLegacyChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", "older-review", review); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i <= changeLegacyReadLimit; i++ {
				id := fmt.Sprintf("legacy-%s-%03d", family, i)
				switch family {
				case "attempts", "side data":
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)`, id, f.change.ID, i+10, `{"attempt_id":"historical"}`); err != nil {
						t.Fatal(err)
					}
				case "policy":
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,'{}')`, id, scope.organization, scope.project, f.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				case "escape":
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'discussion','{}')`, f.change.ID, version.ID); err != nil {
						t.Fatal(err)
					}
				case "comments":
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,?,'comment','{"kind":"runner"}',?,?)`, id, scope.organization, scope.project, f.issue.WorkItemID, i+1, testTimestamp, testTimestamp); err != nil {
						t.Fatal(err)
					}
				case "reviews":
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review','{"decision":"commented"}')`, f.change.ID, version.ID); err != nil {
						t.Fatal(err)
					}
				case "landing attempt":
					seedHealthRateAttempt(t, tx, scope, f.issue, machine, "succeeded", tracker.NativeRunData{}, f.service.config.now(), -1)
				}
			}
			switch family {
			case "reviews":
				var reviews []tracker.ChangeReview
				reviews, _, err = legacyRecoveryReviews(t.Context(), tx, f.change.ID, version.ID)
				if err == nil && (len(reviews) != 33 || reviews[0].ImplementationReview == nil) {
					t.Fatalf("legacy overflow hid older implementation review: %+v", reviews)
				}
			case "attempts":
				var attempts []any
				attempts, err = readLegacyChangeAttempts(t.Context(), tx, scope, string(f.issue.WorkItemID))
				if err == nil && len(attempts) != changeLegacyReadLimit+1 {
					t.Fatalf("lost legacy attempts: %d", len(attempts))
				}
			case "side data":
				issue := f.issue
				err = readLegacyIssueChangeSideData(t.Context(), tx, scope, string(f.issue.WorkItemID), &issue)
			case "policy":
				_, _, err = readNativePolicyLanding(t.Context(), tx, string(scope.organization), string(scope.project), &policy.RepositorySource{Repository: "owner/repository", Commit: strings.Repeat("a", 40), DefaultBranch: "develop"})
			case "escape":
				_, err = readEscapeOccurrence(t.Context(), tx, f.change.ID, version.ID, "missing")
			case "comments":
				_, err = readRecoveryCommentIDs(t.Context(), tx, scope, string(f.issue.WorkItemID), " ORDER BY sequence", false)
			case "landing attempt":
				_, err = latestRecoveryLandingAttempt(t.Context(), tx, scope, string(f.issue.WorkItemID))
			}
			if family == "escape" && errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			if err != nil {
				t.Fatalf("%s legacy read failed: %v", family, err)
			}
			if family == "side data" {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
			}
		})
	}
}

func TestRecoveryUnknownHistoryBudgetPreservesOlderEvidence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		version := f.publish(t, "version", "")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		for _, family := range []string{"check", "landing exit"} {
			t.Run(f.change.ID+"/"+family, func(t *testing.T) {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				at := f.service.config.now()
				if family == "check" {
					check := tracker.ChangeCheck{VersionID: version.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "wanted", Conclusion: "failure"}}
					if err := seedLegacyChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "check", "wanted", check); err != nil {
						t.Fatal(err)
					}
				} else if err := appendNativeHistory(t.Context(), tx, scope, string(f.issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Merging", ToState: "Rework", Reason: "landing_refused"}, at); err != nil {
					t.Fatal(err)
				}
				appendUnknown := func() {
					t.Helper()
					if family == "check" {
						_, err = tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'check','{"check_run_id":"other"}')`, f.change.ID, version.ID)
					} else {
						err = appendNativeHistory(t.Context(), tx, scope, string(f.issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, at)
						if err == nil {
							_, err = tx.ExecContext(t.Context(), `UPDATE collaboration_events SET from_state=NULL WHERE work_item_id=? AND sequence=(SELECT max(sequence) FROM collaboration_events WHERE work_item_id=?)`, f.issue.WorkItemID, f.issue.WorkItemID)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				read := func() (bool, error) {
					if family == "check" {
						checks, err := readRecoveryChangeCheck(t.Context(), tx, f.change.ID, version.ID, "wanted")
						return len(checks) == 1 && checks[0].CheckRunID == "wanted" && checks[0].Conclusion == "failure", err
					}
					exit, found, err := readLatestLandingExit(t.Context(), tx, scope, string(f.issue.WorkItemID))
					return found && exit.Data.Reason == "landing_refused", err
				}
				for range changeLegacyReadLimit - 1 {
					appendUnknown()
				}
				if found, err := read(); err != nil || !found {
					t.Fatalf("boundary hid older evidence: found=%v error=%v", found, err)
				}
				appendUnknown()
				if found, err := read(); err != nil || !found {
					t.Fatalf("legacy overflow hid older evidence: found=%v error=%v", found, err)
				}
				if family == "check" {
					_, err = tx.ExecContext(t.Context(), `UPDATE change_evidence SET check_run_id='other' WHERE change_id=? AND kind='check' AND check_run_id IS NULL`, f.change.ID)
				} else {
					_, err = tx.ExecContext(t.Context(), `UPDATE collaboration_events SET from_state='Todo' WHERE work_item_id=? AND from_state IS NULL`, f.issue.WorkItemID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if found, err := read(); err != nil || !found {
					t.Fatalf("converted history hid older evidence: found=%v error=%v", found, err)
				}
			})
		}
	}
}

func TestValidationCandidatesKeepHistoricalMatchBeyondPage(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	old := f.publish(t, "old", "")
	runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	runner.enroll(t)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	head := strings.Repeat("a", 40)
	for i := 0; i <= changeLegacyReadLimit; i++ {
		version := old
		if i > 0 {
			version.ID = fmt.Sprintf("version-other-%d", i)
			version.Number = int64(i + 1)
			raw, err := marshalNative(version)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)`, version.ID, f.change.ID, version.Number, raw); err != nil {
				t.Fatal(err)
			}
		}
		merge := strings.Repeat("b", 40)
		if i == 0 {
			merge = head
		}
		receipt := tracker.NativeLandingReceipt{VersionID: version.ID, MergeSHA: merge, Landed: true}
		run := tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &receipt}}
		attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", run, f.service.config.now(), -1)
		raw, err := marshalNative(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var projected any = merge
		if i == 0 {
			projected = nil
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_landing_receipts(attempt_id,version_id,record_json,merge_sha) VALUES(?,?,?,?)`, attempt, version.ID, raw, projected); err != nil {
			t.Fatal(err)
		}
	}
	noReceipt := old
	noReceipt.ID = "version-without-receipt"
	noReceipt.Number = changeLegacyReadLimit + 2
	noReceiptRaw, err := marshalNative(noReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)`, noReceipt.ID, f.change.ID, noReceipt.Number, noReceiptRaw); err != nil {
		t.Fatal(err)
	}
	rows, partial, err := readValidationCandidates(f.service.runDetailContext(t.Context()), tx, scope, string(f.issue.WorkItemID), "", "", gate.ScheduledEvidence{Repository: validationRepository(old.Repository), HeadSHA: head})
	if err != nil || !partial || len(rows) != 20 || rows[0].VersionID != old.ID || rows[0].IntegratedSource != "same_commit" || slices.ContainsFunc(rows, func(row tracker.ValidationComparison) bool { return row.VersionID == noReceipt.ID }) {
		t.Fatalf("lost historical match beyond 20 nonmatches: rows=%+v partial=%v error=%v", rows, partial, err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE change_landing_receipts SET merge_sha=NULL WHERE version_id IN (SELECT id FROM change_versions WHERE change_id=?)`, f.change.ID); err != nil {
		t.Fatal(err)
	}
	rows, partial, err = readValidationCandidates(f.service.runDetailContext(t.Context()), tx, scope, string(f.issue.WorkItemID), "", "", gate.ScheduledEvidence{Repository: validationRepository(old.Repository), HeadSHA: head})
	if err != nil || !partial || len(rows) != 20 || rows[0].VersionID != old.ID || rows[0].IntegratedSource != "same_commit" {
		t.Fatalf("legacy receipts hid older match: rows=%+v partial=%v error=%v", rows, partial, err)
	}
}

func TestActivityTypedAndHistoricalAttemptLinks(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	enrolled := prepareRunner(t, first.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	enrolled.enroll(t)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		version := f.publish(t, "version", "")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		at := f.service.config.now().Truncate(time.Second)
		for _, source := range []string{"finalization", "checkpoint", "latest Change"} {
			for _, known := range []bool{false, true} {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				runner := "runner-" + string(f.issue.WorkItemID)
				run := tracker.NativeRunData{RunnerID: runner, SessionID: "recorded-session", Identity: &tracker.NativeExecutionIdentity{Role: "code"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", PhasesDropped: 2, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}}
				wantURL := "https://example.test/pull/42"
				if source == "finalization" {
					run.Finalization = &tracker.NativeFinalization{ChangeID: f.change.ID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: "https://example.test/pull/99"}}}
					wantURL = run.Finalization.Publication.External.URL
					if known {
						run.Finalization.ChangeID = "payload-only-change"
						wantURL = "https://example.test/pull/77"
					}
				}
				attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(enrolled.binding.MachineID), "succeeded", run, at, -1)
				if known {
					if err := updateActivityAttemptProjection(t.Context(), tx, attempt, run, &tracker.NativeCheckpoint{}); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
				}
				checkpoint := "{}"
				if source == "checkpoint" {
					raw, err := json.Marshal(tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: f.change.ID}})
					if err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					checkpoint = string(raw)
					if known {
						checkpoint = `{"change":{"change_id":"payload-only-change"}}`
					}
				}
				var marker, checkpointID, publicationURL any
				finalizationID := ""
				typedRunner := "unconverted"
				if known {
					marker = 1
					publicationURL = ""
					typedRunner = "typed-" + runner
					runner = typedRunner
					checkpointID = ""
					if source == "checkpoint" {
						checkpointID = f.change.ID
					}
					if source == "finalization" {
						finalizationID = f.change.ID
						publicationURL = wantURL
					}
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_json=?,runner_id=?,activity_projection_version=?,finalization_change_id=?,activity_publication_url=?,activity_checkpoint_change_id=? WHERE id=?`, checkpoint, typedRunner, marker, finalizationID, publicationURL, checkpointID, attempt); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE change_versions SET activity_external_url=? WHERE id=?`, "https://example.test/pull/42", version.ID); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if !known {
					if _, err := tx.ExecContext(t.Context(), `UPDATE change_requests SET activity_created_at=NULL,activity_current_version_id=NULL WHERE id=?`, f.change.ID); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
				}
				rows, err := readActivityAttempts(t.Context(), tx, string(scope.organization), []string{string(scope.project)}, operatortool.ActivityRequest{Limit: 5, RunnerID: runner}, operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at.Add(time.Second)}, at.Add(time.Second), false)
				if err != nil || len(rows) != 1 {
					tx.Rollback()
					t.Fatalf("source=%s typed=%v activity=%+v error=%v", source, known, rows, err)
				}
				got := rows[0]
				if got.ChangeID != f.change.ID || got.PullRequestURL != wantURL || got.RunnerID != runner || got.SessionID != run.SessionID || got.Phase != "implementation" || got.Stage != "code" || !got.Partial || got.StageDurationSeconds == nil || *got.StageDurationSeconds != 60 {
					tx.Rollback()
					t.Fatalf("source=%s typed=%v lost rendered evidence: %+v", source, known, got)
				}
				if known && source == "finalization" {
					if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET activity_publication_url=NULL WHERE id=?`, attempt); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					rows, err := readActivityAttempts(t.Context(), tx, string(scope.organization), []string{string(scope.project)}, operatortool.ActivityRequest{Limit: 5, RunnerID: runner}, operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at.Add(time.Second)}, at.Add(time.Second), false)
					if err != nil || len(rows) != 1 || rows[0].ChangeID != f.change.ID || rows[0].PullRequestURL != run.Finalization.Publication.External.URL {
						tx.Rollback()
						t.Fatalf("unknown publication overwrote typed Change or hid payload URL: %+v %v", rows, err)
					}
				}
				tx.Rollback()
			}
		}
	}
	tx, err := first.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{organization: first.project.OrganizationID, project: first.project.ID}
	at := first.service.config.now().Truncate(time.Second)
	for i := 0; i <= changeLegacyReadLimit; i++ {
		seedHealthRateAttempt(t, tx, scope, first.issue, string(enrolled.binding.MachineID), "succeeded", tracker.NativeRunData{RunnerID: "other-runner"}, at, -1)
	}
	rows, err := readActivityAttempts(t.Context(), tx, string(scope.organization), []string{string(scope.project)}, operatortool.ActivityRequest{Limit: 5, RunnerID: "missing-runner"}, operatortool.AnalyticsWindow{From: at.Add(-time.Hour), To: at.Add(time.Second)}, at.Add(time.Second), false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("legacy activity filter failed: rows=%+v error=%v", rows, err)
	}

}

func TestLegacyClaimReviewsRetainGateAuthority(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "initial", "")
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	reviews := []tracker.ChangeReview{
		{ID: "human-approval", Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "approved"},
		{ID: "old-implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "old-validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "source-rejection", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "changes_requested"},
		{ID: "landing-rejection", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "changes_requested"},
	}
	for i := range claimEvidenceLimit + 1 {
		reviews = append(reviews, tracker.ChangeReview{ID: fmt.Sprintf("comment-%d", i), Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "commented"})
	}
	reviews = append(reviews,
		tracker.ChangeReview{ID: "human-refusal", Actor: tracker.Actor{PrincipalID: "second-reviewer"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "fail"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		tracker.ChangeReview{ID: "source-approval", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "approved"},
		tracker.ChangeReview{ID: "landing-approval", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "approved"},
	)
	for _, review := range reviews {
		review.VersionID = version.ID
		if err := seedLegacyChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", "", review); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	selected, err := readClaimReviews(t.Context(), f.service.database.reader, f.change.ID, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(selected))
	for _, review := range selected {
		ids = append(ids, review.ID)
	}
	want := []string{"human-approval", "source-rejection", "landing-rejection", "human-refusal", "implementation", "validator", "source-approval", "landing-approval"}
	if !slices.Equal(ids, want) {
		t.Fatalf("review selection lost gate authority: got %v want %v", ids, want)
	}
}
