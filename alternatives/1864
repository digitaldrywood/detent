package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRetention(t *testing.T) {
	t.Run("claim reviews retain each reviewer across historical backfill", func(t *testing.T) {
		f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), retentionTimeout: frozenRetentionTimeout}))
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		first := f.publish(t, "first", "", true)
		second := f.publish(t, "second", first.ID, true)
		db := f.service.database
		tx, err := db.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for i := range claimEvidenceLimit + 1 {
			review := tracker.ChangeReview{VersionID: first.ID, Actor: tracker.Actor{PrincipalID: "reviewer-one"}, Decision: "commented"}
			if i == 0 {
				review.Decision = "changes_requested"
			}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, f.change.ID, first.ID, string(raw)); err != nil {
				t.Fatal(err)
			}
		}
		for _, version := range []string{first.ID, second.ID} {
			for _, review := range []tracker.ChangeReview{
				{VersionID: version, Actor: tracker.Actor{PrincipalID: "reviewer-one"}, Decision: "approved"},
				{VersionID: version, Actor: tracker.Actor{PrincipalID: "reviewer-two"}, Decision: "changes_requested"},
			} {
				if err := seedLegacyChangeEvidence(t.Context(), tx, f.change.ID, version, "review", "", review); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		reviewsBefore, err := readClaimReviews(t.Context(), db.reader, f.change.ID, first.ID)
		if err != nil || len(reviewsBefore) != 2 || reviewsBefore[0].Decision != "approved" || reviewsBefore[1].Decision != "changes_requested" {
			t.Fatalf("legacy reviews lost current authority: %+v %v", reviewsBefore, err)
		}
		beforeSchema := storageSchemaVersion(t, db.reader)
		for range (claimEvidenceLimit+1)/storageConversionRows + 2 {
			if err := db.populateClaimFacts(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for _, version := range []string{first.ID, second.ID} {
			reviews, err := readClaimReviews(t.Context(), db.reader, f.change.ID, version)
			if err != nil {
				t.Fatal(err)
			}
			decisions := make(map[string]string)
			for _, review := range reviews {
				decisions[review.Actor.PrincipalID] = review.Decision
			}
			if len(reviews) != 2 || decisions["reviewer-one"] != "approved" || decisions["reviewer-two"] != "changes_requested" {
				t.Fatalf("projected reviews lost current authority: %+v", reviews)
			}
		}
		for _, update := range []struct{ actor, decision string }{{"reviewer-two", "approved"}, {"reviewer-one", "changes_requested"}} {
			tx, err := db.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			review := tracker.ChangeReview{VersionID: first.ID, Actor: tracker.Actor{PrincipalID: update.actor}, Decision: update.decision}
			if err := seedLegacyChangeEvidence(t.Context(), tx, f.change.ID, first.ID, "review", "", review); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			reviews, err := readClaimReviews(t.Context(), db.reader, f.change.ID, first.ID)
			if err != nil || !slices.ContainsFunc(reviews, func(review tracker.ChangeReview) bool {
				return review.Actor.PrincipalID == update.actor && review.Decision == update.decision
			}) {
				t.Fatalf("new unknown review lost authority: %+v %v", reviews, err)
			}
			if err := db.populateClaimFacts(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		reviews, err := readClaimReviews(t.Context(), db.reader, f.change.ID, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		decisions := make(map[string]string)
		for _, review := range reviews {
			decisions[review.Actor.PrincipalID] = review.Decision
		}
		if decisions["reviewer-one"] != "changes_requested" || decisions["reviewer-two"] != "approved" {
			t.Fatalf("new review authority was hidden: %+v", reviews)
		}
		if after := storageSchemaVersion(t, db.reader); after != beforeSchema {
			t.Fatalf("claim projection changed schema: %d -> %d", beforeSchema, after)
		}
		if _, err := db.db.ExecContext(t.Context(), `UPDATE change_evidence SET record_json='{}' WHERE change_id=?`, f.change.ID); err == nil {
			t.Fatal("evidence immutability trigger was not restored")
		}
	})

	t.Run("claim facts resume from durable unknown rows", func(t *testing.T) {
		f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), retentionTimeout: frozenRetentionTimeout}))
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		db := f.service.database
		for i := range claimFactsRowsPerTick*3 + 2 {
			id := fmt.Sprintf("legacy-change-%03d", i)
			if _, err := db.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,?)`, id, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, `{}`); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.populateClaimFacts(t.Context()); err != nil {
			t.Fatal(err)
		}
		var unknown int
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_requests WHERE claim_version_id IS NULL`).Scan(&unknown); err != nil || unknown != claimFactsRowsPerTick*2+2 {
			t.Fatalf("unknown=%d: %v", unknown, err)
		}
		for range 3 {
			if err := db.populateClaimFacts(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_requests WHERE claim_version_id IS NULL`).Scan(&unknown); err != nil || unknown != 0 {
			t.Fatalf("unknown=%d: %v", unknown, err)
		}
		for _, row := range []struct{ id, raw string }{
			{"isolated-first", `{}`},
			{"isolated-second", `{"current_version_id":42}`},
		} {
			if _, err := db.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,?)`, row.id, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, row.raw); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.populateClaimFacts(t.Context()); err == nil {
			t.Fatal("malformed claim fact did not fail")
		}
		var projected bool
		if err := db.reader.QueryRowContext(t.Context(), `SELECT claim_version_id IS NOT NULL FROM change_requests WHERE id='isolated-first'`).Scan(&projected); err != nil || !projected {
			t.Fatalf("earlier projection rolled back: projected=%v error=%v", projected, err)
		}
	})

	t.Run("archive error preserves independent compaction", func(t *testing.T) {
		f := newStorageArchiveFixture(t)
		issue := f.nativeFixture.create(t, "archive failure")
		f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
		archiveErr := errors.New("injected archive failure")
		f.hook = func(entryauthority.ContentRequest) error { return archiveErr }
		at := f.now.Add(-31 * 24 * time.Hour)
		record := f.record(f.owner, "old conversation", at)
		record.Status = conversation.StatusSettled
		record.SettledAt = &at
		record.Execution.Status = conversation.ExecutionCompleted
		record = f.create(t, record)
		tx := f.tx(t)
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "final answer", Delivery: conversation.DeliveryCompleted, CreatedAt: at, UpdatedAt: at, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "example-runner"}}
		if err := f.appendMessage(t, t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := f.appendEvent(t, t.Context(), tx, record.ID, conversation.EventMessageDelta, conversationDeltaBody(message.ID, 0, "fragment"), at); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,?,'slots','example',3,'{}')`, f.project.OrganizationID, f.project.ID, at.Truncate(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
		f.commit(t, tx)
		if err := f.service.maintainNativeRetention(t.Context(), f.now); !errors.Is(err, archiveErr) {
			t.Fatalf("retention error=%v", err)
		}
		var deltas, samples, summaries int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_events WHERE conversation_id=? AND type='message.delta'", record.ID).Scan(&deltas); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_hourly_rollups WHERE project_id=? AND metric='slots'", f.project.ID).Scan(&samples); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_sample_hours WHERE project_id=? AND metric='slots'", f.project.ID).Scan(&summaries); err != nil {
			t.Fatal(err)
		}
		if deltas != 0 || samples != 0 || summaries != 1 {
			t.Fatalf("deltas=%d samples=%d summaries=%d", deltas, samples, summaries)
		}
	})
	t.Run("command receipts and storage accounting", func(t *testing.T) {
		now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
		f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, retentionTimeout: frozenRetentionTimeout})
		issue := f.create(t, "receipts")
		var actor string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM api_tokens LIMIT 1").Scan(&actor); err != nil {
			t.Fatal(err)
		}
		counter := func() int64 {
			var value int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := counter()
		base := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects/" + string(f.project.ID)
		receipts := []struct {
			operation, key string
			age            time.Duration
			kept           bool
		}{
			{"POST " + base + "/landing-barrier", "old", 25 * time.Hour, false},
			{"PATCH " + base + "/work-items/x", "old-patch", 25 * time.Hour, false},
			{"POST " + base + "/landing-barrier", "recent", time.Hour, true},
			{"POST " + base + "/conversations", "create-conversation", 25 * time.Hour, true},
			{"POST " + base + "/conversations/c/link", "link", 25 * time.Hour, true},
			{"mcp update_runner_routing", "mcp", 25 * time.Hour, true},
		}
		for _, receipt := range receipts {
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at) VALUES(?,?,?,?,'hash',?,?)", f.project.OrganizationID, actor, receipt.operation, receipt.key, `{"output":"`+fmt.Sprintf("%0512d", 0)+`"}`, formatHubTime(now.Add(-receipt.age))); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt"}}, now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if after := counter(); after != before {
			t.Fatalf("bookkeeping counted against storage: before=%d after=%d", before, after)
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "customer content"})
		requireNativeStatus(t, response, http.StatusOK)
		if counter() <= before {
			t.Fatal("customer comment was not counted")
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		for _, receipt := range receipts {
			var count int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE command_key=?", receipt.key).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != receipt.kept {
				t.Fatalf("receipt %s %s kept=%v", receipt.operation, receipt.key, count == 1)
			}
		}
	})
	t.Run("duplicate challenges", func(t *testing.T) {
		now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
		f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, retentionTimeout: frozenRetentionTimeout})
		other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-challenges")
		for _, project := range []tracker.NativeProject{f.project, other.project} {
			for _, test := range []struct {
				name     string
				age      time.Duration
				consumed *string
			}{
				{"expired", time.Hour + time.Second, nil},
				{"boundary", time.Hour, nil},
				{"live", time.Hour - time.Second, nil},
				{"consumed", 0, new(formatHubTime(now))},
			} {
				token := string(project.ID) + "-" + test.name
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_duplicate_challenges
(token,organization_id,project_id,principal_id,request_id,content_hash,matches_json,consumed_at,created_at)
VALUES (?,?,?,?,?,?,?,?,?)`, token, project.OrganizationID, project.ID, "principal", token, "hash", "[]", test.consumed, formatHubTime(now.Add(-test.age))); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.NativeProject{f.project, other.project} {
			var count int
			var token string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*), COALESCE(min(token),'') FROM native_duplicate_challenges WHERE project_id=?", project.ID).Scan(&count, &token); err != nil {
				t.Fatal(err)
			}
			if count != 1 || token != string(project.ID)+"-live" {
				t.Fatalf("retained challenges=%d, token=%s", count, token)
			}
		}
	})
	t.Run("hourly host history", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
		f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, retentionTimeout: frozenRetentionTimeout})
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
		r.enroll(t)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for index := range 744 {
			summary := testHostSummary(now.Add(-time.Duration(index) * time.Hour))
			raw, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?,'[]')", f.project.OrganizationID, r.binding.RunnerID, formatHubTime(summary.Hour), string(raw)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_host_hours WHERE runner_id=?", r.binding.RunnerID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 720 {
			t.Fatalf("retained %d hours, want 720", count)
		}
		stale := testHostSummary(now.Add(-720 * time.Hour))
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "host_metrics": []hostmetrics.Summary{stale}})
		requireNativeStatus(t, response, http.StatusOK)
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_host_hours WHERE runner_id=?", r.binding.RunnerID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 720 {
			t.Fatalf("stale retry recreated history: %d hours", count)
		}
		history, err := f.service.readRunnerHostHistory(t.Context(), f.project.OrganizationID, r.binding.RunnerID, now.UTC().Truncate(time.Hour).Add(-719*time.Hour), now.UTC().Truncate(time.Hour).Add(time.Hour))
		if err != nil || len(history) != 720 {
			t.Fatalf("bounded history=%d error=%v", len(history), err)
		}
	})

	t.Parallel()
	for _, test := range []struct {
		name, state                                                                                                     string
		age                                                                                                             int
		lease, expiredLease, openChange, landedChange, never, restore, typedRestore, typedOpen, terminalMove, noHistory bool
		period                                                                                                          int
		archived                                                                                                        bool
	}{
		{name: "old Done", state: "Done", age: 31, archived: true},
		{name: "recent Done", state: "Done", age: 29},
		{name: "Done boundary", state: "Done", age: 30},
		{name: "old Cancelled", state: "Cancelled", age: 8, archived: true},
		{name: "recent Cancelled", state: "Cancelled", age: 6},
		{name: "live lease", state: "Done", age: 31, lease: true},
		{name: "expired lease", state: "Done", age: 31, lease: true, expiredLease: true, archived: true},
		{name: "open Change", state: "Done", age: 31, openChange: true},
		{name: "landed Change", state: "Done", age: 31, openChange: true, landedChange: true, archived: true},
		{name: "Never completed", state: "Done", age: 100, never: true},
		{name: "Never cancelled", state: "Cancelled", age: 100, never: true},
		{name: "typed restore resets clock", state: "Done", age: 100, typedRestore: true},
		{name: "typed pending Change", state: "Done", age: 31, openChange: true, landedChange: true, typedOpen: true},
		{name: "restore resets clock", state: "Done", age: 100, restore: true},
		{name: "custom terminal", state: "Shipped", age: 31, archived: true},
		{name: "terminal move preserves clock", state: "Shipped", age: 31, terminalMove: true, archived: true},
		{name: "no terminal history", state: "Done", age: 100, noHistory: true},
		{name: "nonterminal", state: "Todo", age: 100},
		{name: "short completed period", state: "Done", age: 8, period: 7, archived: true},
		{name: "long cancelled period", state: "Cancelled", age: 8, period: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, retentionTimeout: frozenRetentionTimeout})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			issue := f.create(t, "retention")
			if test.lease {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				lease := claimNativeAttempt(t, f, f.worker(t, "worker"), "machine", "session", issue.WorkItemID)
				if test.expiredLease {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now.Add(-time.Hour)), lease.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.never || test.period != 0 {
				current, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
				if err != nil {
					t.Fatal(err)
				}
				field := "archive_completed_after_days"
				if test.state == "Cancelled" {
					field = "archive_cancelled_after_days"
				}
				var period any
				if !test.never {
					period = test.period
				}
				response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, map[string]any{
					"idempotency_key": "set-period", "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled, field: period,
				})
				requireNativeStatus(t, response, http.StatusOK)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if test.state == "Shipped" || test.state == "Cancelled" {
				states := append(append([]tracker.NativeState{}, f.project.States...), tracker.NativeState{Name: test.state, Terminal: true})
				if err := updateNativeProjectStates(t.Context(), tx, scope, states, now); err != nil {
					t.Fatal(err)
				}
			}
			if test.openChange {
				change := tracker.ChangeRequest{ID: "chg_retention", OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, CurrentVersion: "cv_retention"}
				if test.landedChange {
					change.Landed = &tracker.ChangeLanding{VersionID: "cv_retention"}
				}
				raw, err := marshalNative(change)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id) VALUES(?,?,?,?,?,?)", change.ID, scope.organization, scope.project, issue.WorkItemID, raw, change.CurrentVersion); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_issue_links(change_id,organization_id,project_id,work_item_id) VALUES(?,?,?,?)", change.ID, scope.organization, scope.project, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				versionRaw, err := marshalNative(tracker.ChangeVersion{ID: change.CurrentVersion, ChangeID: change.ID, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40)}, Policy: hubTestPolicy()})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,?)", change.CurrentVersion, change.ID, versionRaw); err != nil {
					t.Fatal(err)
				}
				if err := projectChangeColumns(t.Context(), tx, "change_requests", change.ID, raw); err != nil {
					t.Fatal(err)
				}
			}
			if test.state != "Todo" {
				issue.State = test.state
				if test.terminalMove {
					issue.State = "Done"
				}
				event := "workflow.transitioned"
				if test.noHistory {
					event = "issue.edited"
				}
				issue, err = persistNativeIssue(t.Context(), tx, scope, issue, event, tracker.CollaborationData{FromState: "Todo", ToState: issue.State}, now.Add(-time.Duration(test.age)*24*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if test.terminalMove {
					issue.State = test.state
					issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Done", ToState: test.state}, now.Add(-time.Hour))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.restore {
				issue, err = setNativeArchiveTx(t.Context(), tx, scope, string(issue.WorkItemID), issue.Revision, true, now.Add(-2*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				issue, err = setNativeArchiveTx(t.Context(), tx, scope, string(issue.WorkItemID), issue.Revision, false, now.Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.typedOpen {
				if _, err := tx.ExecContext(t.Context(), "UPDATE change_requests SET claim_version_id='new-version',claim_head_sha='new-head',landed_version_id='cv_retention' WHERE id='chg_retention'"); err != nil {
					t.Fatal(err)
				}
			}
			if test.typedRestore {
				if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "issue.edited", tracker.CollaborationData{}, now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET archive_operation='restore' WHERE work_item_id=? AND type='issue.edited'", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			got := readWorkItem(t, f, issue.WorkItemID, "")
			if got.Archived != test.archived {
				t.Fatalf("archived=%v, want %v", got.Archived, test.archived)
			}
			if !test.archived && got.Revision != issue.Revision {
				t.Fatalf("skipped issue changed revision: %d", got.Revision)
			}
			if test.archived {
				if got.Revision != issue.Revision+1 || got.State != issue.State || got.Body != issue.Body {
					t.Fatalf("archive changed content: %+v", got)
				}
				var data, actor string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT data_json,actor_json FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&data, &actor); err != nil {
					t.Fatal(err)
				}
				var event tracker.CollaborationData
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatal(err)
				}
				days := 30
				if test.state == "Cancelled" {
					days = 7
				}
				if test.period != 0 {
					days = test.period
				}
				if event.Operation != "archive" || event.ReasonDetail != fmt.Sprintf("Archived automatically after %d days", days) || actor != `{"kind":"integration","principal_id":"auto_archive"}` {
					t.Fatalf("archive event=%s actor=%s", data, actor)
				}
				if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if repeated := readWorkItem(t, f, issue.WorkItemID, ""); repeated.Revision != got.Revision {
					t.Fatal("sweep archived twice")
				}
			}
			if test.restore || test.lease && !test.expiredLease || test.openChange && !test.landedChange {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE released_at IS NULL", formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				if test.openChange {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_requests SET archive_landed_version_id='cv_retention',record_json=json_set(record_json,'$.landed.version_id','cv_retention') WHERE id='chg_retention'"); err != nil {
						t.Fatal(err)
					}
				}
				retryAt := now.Add(time.Minute)
				if test.restore {
					retryAt = now.Add(31 * 24 * time.Hour)
				}
				if err := f.service.maintainNativeRetention(t.Context(), retryAt); err != nil {
					t.Fatal(err)
				}
				if !readWorkItem(t, f, issue.WorkItemID, "").Archived {
					t.Fatal("eligible retry did not archive")
				}
			}
		})
	}
}

func TestProjectArchivePeriods(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	current, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
	if err != nil {
		t.Fatal(err)
	}
	if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != 30 || current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != 7 {
		t.Fatalf("defaults=%+v", current)
	}
	for index, test := range []struct {
		name                         string
		completed, cancelled         any
		sendCompleted, sendCancelled bool
		status                       int
		wantCompleted, wantCancelled int
	}{
		{name: "set", completed: 14, sendCompleted: true, status: 200, wantCompleted: 14, wantCancelled: 7},
		{name: "never", cancelled: nil, sendCancelled: true, status: 200, wantCompleted: 14},
		{name: "omit preserves", status: 200, wantCompleted: 14},
		{name: "invalid days", completed: 8, sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "string", completed: "30", sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "zero", completed: 0, sendCompleted: true, status: 422, wantCompleted: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"idempotency_key": fmt.Sprint("period-", index), "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled}
			if test.sendCompleted {
				payload["archive_completed_after_days"] = test.completed
			}
			if test.sendCancelled {
				payload["archive_cancelled_after_days"] = test.cancelled
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
			requireNativeStatus(t, response, test.status)
			current, err = readProjectIntegration(t.Context(), f.service.database.db, scope)
			if err != nil {
				t.Fatal(err)
			}
			if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != test.wantCompleted {
				t.Fatalf("completed=%v", current.ArchiveCompletedAfterDays)
			}
			if test.wantCancelled == 0 && current.ArchiveCancelledAfterDays != nil || test.wantCancelled != 0 && (current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != test.wantCancelled) {
				t.Fatalf("cancelled=%v", current.ArchiveCancelledAfterDays)
			}
		})
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "settings during work")
	claimNativeAttempt(t, f, f.worker(t, "settings-worker"), "machine", "session", issue.WorkItemID)
	payload := map[string]any{"idempotency_key": "live-settings", "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled, "archive_completed_after_days": 60}
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
	requireNativeStatus(t, response, http.StatusOK)
	var updated ProjectIntegration
	decodeHubResponse(t, response, &updated)
	if updated.ArchiveCompletedAfterDays == nil || *updated.ArchiveCompletedAfterDays != 60 {
		t.Fatalf("live setting not persisted: %+v", updated)
	}
	payload["idempotency_key"] = "live-workflow"
	payload["expected_revision"] = fmt.Sprint(updated.Revision)
	payload["states"] = f.project.States
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload), http.StatusUnprocessableEntity)

	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='acme/checkout-only' WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	checkout, err := readProjectIntegration(t.Context(), f.service.database.db, scope)
	if err != nil {
		t.Fatal(err)
	}
	delete(payload, "states")
	for _, test := range []struct {
		name   string
		intake string
		days   int
	}{
		{"checkout-only-period", "disabled", 60},
		{"checkout-only-omitted-intake", "", 14},
	} {
		payload["idempotency_key"] = test.name
		payload["expected_revision"] = fmt.Sprint(checkout.Revision)
		payload["archive_completed_after_days"] = test.days
		if test.intake == "" {
			delete(payload, "intake")
		} else {
			payload["intake"] = test.intake
		}
		response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &checkout)
		if checkout.Intake != "automatic" || checkout.ArchiveCompletedAfterDays == nil || *checkout.ArchiveCompletedAfterDays != test.days {
			t.Fatalf("archive-only save = %+v", checkout)
		}
		var intake string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT github_intake FROM projects WHERE id=?", f.project.ID).Scan(&intake); err != nil {
			t.Fatal(err)
		}
		if intake != "disabled" {
			t.Fatalf("archive-only save changed integration authority: %s", intake)
		}
	}

}

func TestNativeStorageArchiveRetention(t *testing.T) {
	for _, test := range []struct {
		name, state string
		age         time.Duration
		want        bool
	}{
		{"done before", "Done", 72*time.Hour - time.Second, false},
		{"done boundary", "Done", 72 * time.Hour, true},
		{"done after", "Done", 72*time.Hour + time.Second, true},
		{"cancelled before", "Cancelled", 72*time.Hour - time.Second, false},
		{"cancelled boundary", "Cancelled", 72 * time.Hour, true},
		{"cancelled after", "Cancelled", 73 * time.Hour, true},
		{"open", "Todo", 96 * time.Hour, false},
		{"not started", "Backlog", 96 * time.Hour, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			objects := map[string][]byte{}
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, Content: func(_ context.Context, r entryauthority.ContentRequest) ([]byte, error) {
				if r.Write {
					objects[r.Reference.Key] = slices.Clone(r.Data)
					return nil, nil
				}
				return objects[r.Reference.Key], nil
			}})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			issue := f.create(t, "archive cutoff")
			if test.state != "Todo" {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if test.state == "Cancelled" || test.state == "Backlog" {
					states := append(append([]tracker.NativeState{}, f.project.States...), tracker.NativeState{Name: test.state, Terminal: test.state == "Cancelled"})
					if err := updateNativeProjectStates(t.Context(), tx, scope, states, now); err != nil {
						t.Fatal(err)
					}
				}
				issue.State = test.state
				issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: test.state}, now.Add(-test.age))
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.archiveClosedNativeIssues(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_storage_archives WHERE work_item_id=?", issue.WorkItemID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != test.want {
				t.Fatalf("archives=%d want=%v", count, test.want)
			}
			got := readWorkItem(t, f, issue.WorkItemID, "")
			if got.Archived || got.Revision != issue.Revision {
				t.Fatalf("storage archival changed board issue: %+v", got)
			}
			if test.want {
				ref, err := scanAttachmentArchive(f.service.database.reader.QueryRowContext(t.Context(), `SELECT a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class,a.storage_binding_id,a.customer_owned FROM issue_storage_archives p JOIN attachments a ON a.id=p.attachment_id WHERE p.work_item_id=?`, issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				if ref.Validate(string(scope.organization)) != nil || artifact.Digest(objects[ref.Key]) != ref.SHA256 || int64(len(objects[ref.Key])) != ref.CompressedSize || int64(len(archiveTestBytes(t, objects[ref.Key]))) != ref.UncompressedSize {
					t.Fatalf("invalid root pointer: %+v", ref)
				}
				if err := f.service.archiveClosedNativeIssues(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if count := len(storageTestArchiveObjects(t, f.service, objects)); count != 1 {
					t.Fatalf("repeat publication created %d objects", count)
				}
			}
		})
	}
}

func TestBackgroundStorageCancellationProgress(t *testing.T) {
	for _, test := range []struct {
		name       string
		archive    bool
		aggregates int
		committed  []int
	}{
		{"conversion aggregates", false, 3, []int{3}},
		{"conversion rows", false, 1, []int{2, 3}},
		{"archive", true, 3, []int{3}},
	} {
		archive := test.archive
		t.Run(test.name, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			var issue tracker.NativeIssue
			for index := range 3 {
				if index < test.aggregates {
					issue = f.nativeFixture.create(t, fmt.Sprintf("background progress %d", index))
				}
				if archive {
					f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-73*time.Hour))
				} else {
					tx := f.tx(t)
					if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: strings.Repeat("output", 2048)}}, f.now); err != nil {
						t.Fatal(err)
					}
					f.commit(t, tx)
				}
			}
			count := func() int {
				t.Helper()
				query := `SELECT count(*) FROM collaboration_events WHERE payload_version=1 AND type IN ('run.started','run.checkpointed','run.observed','run.finished')`
				if archive {
					query = `SELECT count(*) FROM issue_storage_archives`
				}
				var value int
				if err := f.service.database.reader.QueryRowContext(t.Context(), query).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			run := func(ctx context.Context) error {
				if archive {
					return f.service.archiveClosedNativeIssues(ctx, f.now)
				}
				return f.service.convertLegacyStorage(ctx, f.now)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writes := 0
			var first, interrupted string
			f.hook = func(r entryauthority.ContentRequest) error {
				if !r.Write {
					return nil
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET name=name WHERE id=?", f.project.ID); err != nil {
					return err
				}
				if archive && !strings.HasSuffix(r.Reference.Key, "/issue.json.gz") {
					return nil
				}
				writes++
				if got := count(); got != writes-1 {
					t.Fatalf("committed units=%d before unit %d", got, writes)
				}
				if writes == 1 {
					first = r.Reference.Key
				} else {
					interrupted = r.Reference.Key
					cancel()
				}
				return nil
			}
			if err := run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled run: %v", err)
			}
			if got := count(); got != 1 {
				t.Fatalf("committed progress after cancellation=%d", got)
			}
			resumed := 0
			f.hook = func(r entryauthority.ContentRequest) error {
				if !r.Write || archive && !strings.HasSuffix(r.Reference.Key, "/issue.json.gz") {
					return nil
				}
				if r.Reference.Key == first {
					t.Fatal("repeated committed unit")
				}
				if resumed == 0 && r.Reference.Key == interrupted {
					t.Fatal("interrupted unit starved unattempted work")
				}
				if test.aggregates == 1 && resumed == 1 && r.Reference.Key != interrupted {
					t.Fatal("wrapped pass did not revisit interrupted unit")
				}
				resumed++
				return nil
			}
			for tick, want := range test.committed {
				f.now = f.now.Add(time.Minute)
				if err := run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := count(); got != want || resumed != want-1 {
					t.Fatalf("tick=%d resumed units=%d committed=%d want=%d", tick, resumed, got, want)
				}
			}
		})
	}
}

func TestNativeRetentionCommitsEachIssue(t *testing.T) {
	f := newStorageArchiveFixture(t)
	for index := range 3 {
		issue := f.nativeFixture.create(t, fmt.Sprintf("retention progress %d", index))
		f.transition(t, string(issue.WorkItemID), "Done", f.now.Add(-31*24*time.Hour))
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER interrupt_archive BEFORE UPDATE OF archived ON issues WHEN NEW.archived=1 AND (SELECT count(*) FROM issues WHERE archived=1)=1 BEGIN SELECT RAISE(ABORT,'interrupted archive'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.expireNativeRecords(t.Context(), f.now); err == nil {
		t.Fatal("expected archive interruption")
	}
	for _, want := range []int{1, 3} {
		if want == 3 {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER interrupt_archive"); err != nil {
				t.Fatal(err)
			}
			if err := f.service.expireNativeRecords(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE archived=1").Scan(&count); err != nil || count != want {
			t.Fatalf("archived=%d want=%d err=%v", count, want, err)
		}
	}
}
