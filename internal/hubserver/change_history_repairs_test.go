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

func TestChangeCommentBackfillPreservesConcurrentEdit(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		id := newNativeID("cmt")
		raw := `{"kind":"runner","principal_id":"author"}`
		at := formatHubTime(f.service.config.now())
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,1,'original',?,?,?)`, id, scope.organization, scope.project, f.issue.WorkItemID, raw, at, at); err != nil {
			t.Fatal(err)
		}
		stale, err := changeRecordColumns(t.Context(), f.service.database.reader, "native_comments", raw)
		if err != nil {
			t.Fatal(err)
		}
		stale.add("change_edited_by_kind", "")
		path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/comments/" + id
		response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-" + id}, ExpectedRevision: 1, Body: "human edit"})
		requireNativeStatus(t, response, http.StatusOK)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeChangeColumns(t.Context(), tx, changeColumnFamily{table: "native_comments"}, id, raw, stale, true); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		newer := newNativeID("cmt")
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at,change_actor_kind,change_edited_by_kind) VALUES(?,?,?,?,1,2,'newer machine comment',?,?,?,'runner','')`, newer, scope.organization, scope.project, f.issue.WorkItemID, raw, at, at); err != nil {
			t.Fatal(err)
		}
		var editor string
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT change_edited_by_kind FROM native_comments WHERE id=?`, id).Scan(&editor); err != nil {
			t.Fatal(err)
		}
		if editor != "human" {
			t.Fatalf("stale backfill overwrote human editor: %q", editor)
		}
		ids, err := readRecoveryCommentIDs(t.Context(), f.service.database.reader, scope, string(f.issue.WorkItemID), " ORDER BY sequence", false)
		if err != nil || !slices.Contains(ids, id) {
			t.Fatalf("human-edited comment missing from recovery: %v %v", ids, err)
		}
	}
}

func TestChangeMixedBackfillKeepsTypedIssueData(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		old := f.publish(t, "old", "")
		current := f.publish(t, "current", old.ID)
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `UPDATE change_versions SET head_sha=NULL WHERE id=?`, old.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE change_versions SET head_sha='typed-current',external_provider='github',external_id='42',external_url='https://example.test/pull/42' WHERE id=?`, current.ID); err != nil {
			t.Fatal(err)
		}
		issue, _, err := readNativeIssueProjection(t.Context(), tx, scope, string(f.issue.WorkItemID), false)
		if err != nil {
			t.Fatal(err)
		}
		if issue.ListChange == nil || issue.ListChange.HeadSHA != "typed-current" || issue.PullRequest == nil || issue.PullRequest.Number != 42 || issue.PullRequest.URL != "https://example.test/pull/42" {
			t.Fatalf("unrelated unknown version replaced typed selections: %+v %+v", issue.ListChange, issue.PullRequest)
		}
		tx.Rollback()
	}
}

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
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", "older-review", review); err != nil {
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
					if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "check", "wanted", check); err != nil {
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
	rows, partial, err := readValidationCandidates(t.Context(), tx, scope, string(f.issue.WorkItemID), "", "", gate.ScheduledEvidence{Repository: validationRepository(old.Repository), HeadSHA: head})
	if err != nil || !partial || len(rows) != 20 || rows[0].VersionID != old.ID || rows[0].IntegratedSource != "same_commit" || slices.ContainsFunc(rows, func(row tracker.ValidationComparison) bool { return row.VersionID == noReceipt.ID }) {
		t.Fatalf("lost historical match beyond 20 nonmatches: rows=%+v partial=%v error=%v", rows, partial, err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE change_landing_receipts SET merge_sha=NULL WHERE version_id IN (SELECT id FROM change_versions WHERE change_id=?)`, f.change.ID); err != nil {
		t.Fatal(err)
	}
	rows, partial, err = readValidationCandidates(t.Context(), tx, scope, string(f.issue.WorkItemID), "", "", gate.ScheduledEvidence{Repository: validationRepository(old.Repository), HeadSHA: head})
	if err != nil || !partial || len(rows) != 20 || rows[0].VersionID != old.ID || rows[0].IntegratedSource != "same_commit" {
		t.Fatalf("legacy receipts hid older match: rows=%+v partial=%v error=%v", rows, partial, err)
	}
}

func TestProductivityRefreshServesChangeBackfill(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		version := f.publish(t, "version", "")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		change := f.changeWithCurrentVersion(t)
		at := f.service.config.now()
		change.Landed = &tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("a", 40), LandedAt: at}
		raw, err := marshalNative(change)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_requests SET record_json=? WHERE id=?`, raw, f.change.ID); err != nil {
			t.Fatal(err)
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := projectChangeColumns(t.Context(), tx, "change_requests", f.change.ID, raw); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", "refusal", tracker.ChangeReview{VersionID: version.ID, Decision: "changes_requested", CreatedAt: at.Add(-time.Hour)}); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		for i := range changeLegacyReadLimit + 1 {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)`, fmt.Sprintf("legacy-%s-%d", f.change.ID, i), f.change.ID, i+100, fmt.Sprintf(`{"created_at":%q}`, formatHubTime(at))); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review','{"decision":"commented"}')`, f.change.ID, version.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"before", "during", "after"} {
			if state == "before" {
				for _, statement := range []string{`UPDATE change_requests SET created_at=NULL,landed_merge_sha=NULL WHERE id=?`, `UPDATE change_versions SET head_sha=NULL WHERE change_id=?`, `UPDATE change_evidence SET has_validator=NULL,claim_review_decision=NULL WHERE change_id=?`} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, f.change.ID); err != nil {
						t.Fatal(err)
					}
				}
			} else if state == "during" {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := projectChangeColumns(t.Context(), tx, "change_requests", f.change.ID, raw); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else if err := f.service.populateChangeColumns(t.Context()); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			inputs, readErr := productivityQualityInputs(t.Context(), tx, scope, string(f.issue.WorkItemID))
			refreshErr := refreshProductivitySummary(t.Context(), tx, scope, string(f.issue.WorkItemID))
			if readErr != nil || refreshErr != nil || len(inputs.currentLandings) != 1 || !slices.ContainsFunc(inputs.events, func(event qualityHistoryEvent) bool { return event.Kind == "changes_requested" }) {
				tx.Rollback()
				t.Fatalf("%s report lost legacy landing or refusal: %+v read=%v refresh=%v", state, inputs, readErr, refreshErr)
			}
			tx.Rollback()
			if state == "before" {
				drained := false
				for range 100 {
					if err := f.service.database.drainAnalyticsItem(t.Context(), string(f.issue.WorkItemID), at); err != nil {
						t.Fatalf("legacy analytics drain: %v", err)
					}
					var dirty bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM analytics_rollup_dirty WHERE work_item_id=?)`, f.issue.WorkItemID).Scan(&dirty); err != nil {
						t.Fatal(err)
					}
					if !dirty {
						drained = true
						break
					}
				}
				if !drained {
					t.Fatal("legacy analytics drain did not finish")
				}
				snapshot, err := readHealthSnapshot(t.Context(), f.service.database.reader, scope.organization, at)
				if err != nil {
					t.Fatalf("legacy health snapshot: %v", err)
				}
				if err := f.service.commitHealthEvaluation(t.Context(), scope.organization, at, evaluateHealth(at, snapshot), snapshot.BaselineUnavailable); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE native_id=?`, scope.project, f.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		appendTestHealthEventAs(t, f.nativeFixture, tracker.Actor{Kind: "human", PrincipalID: "completion-person", DisplayName: "Example person"}, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "In Progress", ToState: "Done"}, at)
		appendTestHealthEventAs(t, f.nativeFixture, tracker.Actor{Kind: "human", PrincipalID: "other-person"}, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, at.Add(time.Minute))
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET from_state=NULL,to_state=NULL WHERE work_item_id=? AND type='workflow.transitioned'`, f.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		tx, err = f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		refreshErr := refreshProductivitySummary(t.Context(), tx, scope, string(f.issue.WorkItemID))
		tx.Rollback()
		if refreshErr != nil {
			t.Fatalf("legacy completion actor unavailable: %v", refreshErr)
		}
		if err := f.service.populateChangeColumns(t.Context()); err != nil {
			t.Fatal(err)
		}
		tx, err = f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := refreshProductivitySummary(t.Context(), tx, scope, string(f.issue.WorkItemID)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		var kind, person string
		err = tx.QueryRowContext(t.Context(), `SELECT completed_kind,completed_id FROM report_issue_summaries WHERE work_item_id=?`, f.issue.WorkItemID).Scan(&kind, &person)
		tx.Rollback()
		if err != nil || kind != "people" || person != "completion-person" {
			t.Fatalf("backfilled completion actor lost: %s %s %v", kind, person, err)
		}
	}
}

func TestChangeVersionBackfillRestoresLinkedPRActivity(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "historical-version")
	other := f.create(t, "other linked issue")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_issue_links(change_id,organization_id,project_id,work_item_id) VALUES(?,?,?,?)`, f.change.ID, scope.organization, scope.project, other.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_versions SET head_sha=NULL,external_url=NULL WHERE id=?`, version.ID); err != nil {
		t.Fatal(err)
	}
	at := f.service.config.now().Add(time.Hour)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE pull_requests SET issue_id=NULL,synchronized_at=? WHERE repository_id=?`, formatHubTime(at), f.repositoryID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []tracker.NativeWorkItemID{f.issue.WorkItemID, other.WorkItemID} {
		var value string
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT last_activity_at FROM issues WHERE organization_id=? AND project_id=? AND native_id=?`, scope.organization, scope.project, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		got, err := parseTimeValue(value)
		if err != nil || !got.Equal(at) {
			t.Fatalf("version backfill lost linked PR synchronization: %s %v", value, err)
		}
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

func TestChangeAttemptBackfillKeepsCurrentCheckpoint(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	runner := prepareRunner(t, first.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	runner.enroll(t)
	for _, f := range []changeFixture{first, first.anotherChange(t)} {
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		run := tracker.NativeRunData{RunnerID: "recorded-runner", Finalization: &tracker.NativeFinalization{ChangeID: f.change.ID, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: "https://example.test/pull/42"}}}}
		raw, err := marshalNative(run)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", run, f.service.config.now(), -1)
		checkpoint := sql.NullString{String: `{"change":{"change_id":"initial"}}`, Valid: true}
		if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_json=? WHERE id=?`, checkpoint, attempt); err != nil {
			t.Fatal(err)
		}
		columns, err := changeRecordColumns(t.Context(), tx, "native_attempts", raw)
		if err != nil {
			t.Fatal(err)
		}
		columns.values[len(columns.values)-1] = "initial"
		columns.checkpointSource = &checkpoint
		if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_json='{"change":{"change_id":"current"}}',checkpoint_change_id='current' WHERE id=?`, attempt); err != nil {
			t.Fatal(err)
		}
		if err := writeChangeColumns(t.Context(), tx, changeColumnFamily{table: "native_attempts"}, attempt, raw, columns, true); err != nil {
			t.Fatal(err)
		}
		var current string
		var ready sql.NullBool
		if err := tx.QueryRowContext(t.Context(), `SELECT checkpoint_change_id,activity_identity_known FROM native_attempts WHERE id=?`, attempt).Scan(&current, &ready); err != nil {
			t.Fatal(err)
		}
		if current != "current" || ready.Valid {
			t.Fatalf("stale checkpoint marked activity projection complete: %s %+v", current, ready)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := f.service.populateChangeColumns(t.Context()); err != nil {
			t.Fatal(err)
		}
		var finalization, url, runnerID string
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT checkpoint_change_id,activity_identity_known,finalization_change_id,finalization_external_url,runner_id FROM native_attempts WHERE id=?`, attempt).Scan(&current, &ready, &finalization, &url, &runnerID); err != nil {
			t.Fatal(err)
		}
		if !ready.Valid || !ready.Bool || current != "current" || finalization != f.change.ID || url != "https://example.test/pull/42" || runnerID != "recorded-runner" {
			t.Fatalf("fresh activity backfill lost identity: %s %+v %s %s %s", current, ready, finalization, url, runnerID)
		}
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
		if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", "", review); err != nil {
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
