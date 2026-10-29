package hubserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportTypedQueryCompatibility(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 7, time.UTC)
	first := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	if err := first.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "report-other")
	window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
	for _, f := range []nativeFixture{first, second} {
		issue := f.create(t, "Example typed reports")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		machine := "machine-" + string(scope.project)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,'example-runner',1,'test',?,?,?)`, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		filer, label := "filer-"+string(scope.project), "label-"+string(scope.project)
		actorRaw, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: filer})
		if err != nil {
			t.Fatal(err)
		}
		labelsRaw, err := json.Marshal([]string{label})
		if err != nil {
			t.Fatal(err)
		}
		var issueID int64
		if err := tx.QueryRowContext(t.Context(), "UPDATE issues SET actor_json=?,labels_json=? WHERE native_id=? RETURNING id", string(actorRaw), string(labelsRaw), issue.WorkItemID).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		if err := updateIssueActor(t.Context(), tx, issueID, tracker.Actor{Kind: "human", PrincipalID: filer}); err != nil {
			t.Fatal(err)
		}
		if err := replaceIssuePageFilterValues(t.Context(), tx, issueID, []string{label}, nil); err != nil {
			t.Fatal(err)
		}
		type attempt struct {
			id, model, stage, raw, changed string
			data                           tracker.NativeRunData
		}
		var attempts []attempt
		for index, identity := range []struct{ role, requested, resolved, execution string }{{"code", "requested", "resolved", ""}, {"rework", "requested-only", "", ""}, {"", "", "", ""}, {"", "", "", "code"}} {
			data := tracker.NativeRunData{RunnerID: fmt.Sprintf("runner-%s-%d", scope.project, index), Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: identity.role, RequestedModel: agentidentity.Value{Value: identity.requested}, ResolvedModel: agentidentity.Value{Value: identity.resolved}}}}
			if identity.execution != "" {
				data.Identity = &tracker.NativeExecutionIdentity{Role: identity.execution}
			}
			status := "running"
			switch index {
			case 0:
				status = "succeeded"
				data.Runtime.Landing = &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now}
			case 1:
				status = "failed"
				data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "attempt failure"}
			}
			id := seedHealthRateAttempt(t, tx, scope, issue, machine, status, data, now.Add(time.Duration(index)*time.Nanosecond), -1)
			if err := updateActivityAttemptProjection(t.Context(), tx, id, data, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			model, stage := identity.resolved, identity.role
			if model == "" {
				model = identity.requested
			}
			if model == "" {
				model = reportsUnavailableName
			}
			if stage == "" {
				stage = reportsUnavailableName
			}
			changed := data
			changed.RunnerID = "payload-runner"
			runtime := *data.Runtime
			runtime.Identity.Role = "payload-stage"
			runtime.Identity.RequestedModel.Value = "payload-requested"
			runtime.Identity.ResolvedModel.Value = "payload-resolved"
			if runtime.Landing != nil {
				landing := *runtime.Landing
				landing.ObservedAt = window.To.Add(time.Hour)
				runtime.Landing = &landing
			}
			changed.Runtime = &runtime
			changedRaw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			attempts = append(attempts, attempt{id, model, stage, string(raw), string(changedRaw), data})
		}
		if err := refreshAnalyticsAttempts(t.Context(), tx, nil, string(scope.organization), string(scope.project), string(issue.WorkItemID)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unknown=%t", scope.project, unknown), func(t *testing.T) {
				for _, a := range attempts {
					statement, raw := "UPDATE native_attempts SET data_json=? WHERE id=?", a.changed
					if unknown {
						statement, raw = "UPDATE native_attempts SET report_projection_version=NULL,runner_id='',role=NULL,report_requested_model=NULL,report_resolved_model=NULL,report_failed_at=NULL,has_landing_receipt=NULL,data_json=? WHERE id=?", a.raw
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, raw, a.id); err != nil {
						t.Fatal(err)
					}
				}
				statement, actor, labels := "UPDATE issues SET actor_json=?,labels_json=? WHERE id=?", "{}", "[]"
				if unknown {
					statement, actor, labels = "UPDATE issues SET actor_principal_id=NULL,page_filters_ready=NULL,actor_json=?,labels_json=? WHERE id=?", string(actorRaw), string(labelsRaw)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), statement, actor, labels, issueID); err != nil {
					t.Fatal(err)
				}
				for _, a := range attempts {
					ctx, err := withReportsScope(t.Context(), scope, reportsFilter{FiledBy: []string{filer}, Labels: []string{label}, Runners: []string{a.data.RunnerID}, Models: []string{a.model}, Stages: []string{a.stage}}, window.From, window.To)
					if err != nil {
						t.Fatal(err)
					}
					for _, q := range []nativeQueryer{f.service.database.reader, analyticsRollupReader{f.service.database.reader}} {
						population, err := loadNativeAnalyticsAttempts(ctx, q, scope, window, a.data.RunnerID)
						if err != nil || len(population.Items) != 1 || population.Items[0].AttemptID != a.id {
							t.Fatalf("typed filters (%T): items=%v error=%v", q, population.Items, err)
						}
						identity := population.Items[0].Identity
						if identity.Role != a.data.Runtime.Identity.Role || identity.RequestedModel.Value != a.data.Runtime.Identity.RequestedModel.Value || identity.ResolvedModel.Value != a.data.Runtime.Identity.ResolvedModel.Value {
							t.Fatalf("typed identity changed: %+v", identity)
						}
						other, err := loadNativeAnalyticsAttempts(ctx, q, scope, window, "unselected-runner")
						if err != nil || len(other.Items) != 0 {
							t.Fatalf("runner filter authorized an unmatched row: %+v %v", other, err)
						}
					}
				}
				failures, partial, err := readNativeFailures(t.Context(), f.service.database.reader, scope, string(issue.WorkItemID), &window)
				if err != nil || partial || len(failures) != 2 || failures[0].attemptID != attempts[1].id || failures[1].attemptID != attempts[0].id || !failures[1].at.Equal(now) {
					t.Fatalf("typed failure selection, nanosecond order or timestamp changed: %+v partial=%t error=%v", failures, partial, err)
				}
				quality, err := loadAnalyticsQualityInputs(t.Context(), f.service.database.reader, scope, window)
				if err != nil {
					t.Fatal(err)
				}
				worked := 0
				for _, event := range quality.events {
					if event.Kind == "worked" {
						worked++
					}
				}
				if worked != 3 {
					t.Fatalf("quality ignored typed runtime roles: worked=%d", worked)
				}
				boundary := window
				boundary.From, boundary.To = now, now.Add(time.Nanosecond)
				failures, _, err = readNativeFailures(t.Context(), f.service.database.reader, scope, "", &boundary)
				if err != nil || len(failures) != 1 || failures[0].attemptID != attempts[0].id {
					t.Fatalf("failure window boundary changed: %+v %v", failures, err)
				}
				landings := []nativeAnalyticsLanding{{WorkItemID: issue.WorkItemID, Landing: tracker.ChangeLanding{LandedAt: window.To}}}
				receipts, err := readLandingAttemptReceipts(t.Context(), f.service.database.reader, scope, landings)
				if err != nil || len(receipts) != 1 || len(receipts[0]) != 1 {
					t.Fatalf("typed landing presence lost: %v %v", receipts, err)
				}
			})
		}
	}
}

func TestReportUnknownFactsServeDuringBackfill(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	issue := f.create(t, "Example unknown reports")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, family := range []string{"report_events", "report_attempts"} {
		t.Run(family, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if family == "report_events" {
				for index := range storageConversionRows + 1 {
					if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", Reason: strings.Repeat("x", 11000)}}, f.service.config.now().Add(time.Duration(index)*time.Nanosecond)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET analytics_projection_version=NULL WHERE work_item_id=?", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			} else {
				now := f.service.config.now()
				machine := "machine-report-unknown"
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,'example-runner',1,'test',?,?,?)`, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				for range storageConversionRows + 1 {
					seedHealthRateAttempt(t, tx, scope, issue, machine, "failed", tracker.NativeRunData{RunnerID: strings.Repeat("example-", 1500)}, now, -1)
				}
			}
			q, err := prepareReportsQuery(t.Context(), tx, scope, family)
			if err != nil {
				t.Fatalf("pending report backfill prevents reads: %v", err)
			}
			var count int
			if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM "+family).Scan(&count); err != nil || count < storageConversionRows+1 {
				t.Fatalf("legacy report rows=%d error=%v", count, err)
			}
		})
	}
}
