package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthFindingOpenOwner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, instance string
		legacy, held          bool
	}{
		{name: "fingerprint"},
		{name: "dependency"},
		{name: "related"},
		{name: "reverse related"},
		{name: "Done", state: "Done"},
		{name: "legacy Done", state: "Done", legacy: true},
		{name: "legacy Cancelled", state: "Cancelled", legacy: true},
		{name: "legacy held Done", state: "Done", legacy: true, held: true},
		{name: "held Done", state: "Done", held: true},
		{name: "unrelated legacy Done", state: "Done", legacy: true, instance: "other_detector"},
		{name: "unowned"},
		{name: "concurrent"},
	} {
		kind := test.name
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-owner")
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "health-owner-other")
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			var findings []healthFinding
			owners := map[tracker.ProjectID]tracker.NativeWorkItemID{}
			previous := map[tracker.ProjectID]tracker.NativeIssue{}
			for _, fixture := range []nativeFixture{f, other} {
				addHealthBacklog(t, fixture)
				workflow := config.Workflow{Config: config.Default(), SharedPrompt: "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n\nAgent-authored criteria do not require human confirmation.\n"}
				workflow.Config.Tracker.Kind = config.TrackerHubNative
				descriptor, err := config.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				approveHubTestPolicy(t, f.service, fixture.base+"/policy", descriptor)
				var subject tracker.NativeIssue
				if kind != "reverse related" {
					subject = fixture.create(t, "affected subject")
				}
				finding := newHealthFinding("recovery_loop", "instance", "work_item", string(subject.WorkItemID), "Repeated checkpoint recovery.", "Inspect retained attempts.", []string{string(fixture.project.ID)}, healthEvidence{Counts: map[string]int{"recoveries": 5}, AttemptIDs: []string{"attempt-evidence"}, EventIDs: []string{"event-evidence"}})
				findings = append(findings, finding)
				if kind == "unowned" || kind == "concurrent" {
					continue
				}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				scope := healthIssueScope(f.project.OrganizationID, fixture.project.ID)
				if test.instance != "" {
					scope.sourceActor = nativeIntegrationActor(test.instance)
				}
				request := tracker.CreateIssue{Title: "Existing repair owner", Body: nativeContractTestBody, State: "Todo"}
				if test.legacy {
					evidence, err := json.MarshalIndent(finding.Evidence, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					request.Body = fmt.Sprintf("Finding: `earlier-finding`\nClass: instance\nSubject: work_item %s\nOpened: %s\nObserved: %s\n\n%s\n\nNext action: %s\n\nEvidence:\n```json\n%s\n```\n\n```detent-agent\nschema: 1\neffort: high\n```\n\nDiagnostic report only. This finding does not establish a reproducible source repair.\n\n<!-- detent:no-dispatch -->", subject.WorkItemID, formatHubTime(now.Add(-time.Hour)), formatHubTime(now.Add(-time.Hour)), finding.Summary, finding.NextAction, evidence)
				}
				if test.held {
					request.Body += "\n\n<!-- detent:no-dispatch -->"
				}
				if kind == "fingerprint" || test.state != "" {
					request.Body = issueorigin.Stamp(request.Body, issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: "earlier-finding", Fingerprint: finding.Fingerprint})
				}
				if test.state != "" {
					request.State = test.state
				}
				if test.state == "Cancelled" {
					project, err := readNativeProject(t.Context(), tx, scope)
					if err != nil {
						t.Fatal(err)
					}
					states, err := json.Marshal(append(project.States, tracker.NativeState{Name: "Cancelled", Terminal: true}))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(states), fixture.project.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", fixture.project.ID, formatHubTime(now), formatHubTime(now)); err != nil {
						t.Fatal(err)
					}
				}
				owner, err := createNativeIssueTx(t.Context(), tx, scope, request, now.Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				owners[fixture.project.ID] = owner.WorkItemID
				previous[fixture.project.ID] = owner
				if kind == "reverse related" {
					subject, err = createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "Affected subject", Body: nativeContractTestBody, State: "Todo"}, now)
					if err != nil {
						t.Fatal(err)
					}
					findings[len(findings)-1] = newHealthFinding(finding.Signal, finding.Class, finding.Subject.Kind, string(subject.WorkItemID), finding.Summary, finding.NextAction, finding.Projects, finding.Evidence)
				}
				if strings.Contains(kind, "related") || kind == "dependency" {
					_, subjectID, err := readNativeIssue(t.Context(), tx, scope, string(subject.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					_, ownerID, err := readNativeIssue(t.Context(), tx, scope, string(owner.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					if kind == "dependency" {
						_, err = tx.ExecContext(t.Context(), "INSERT INTO issue_dependencies(blocker_issue_id,dependent_issue_id,provenance,created_at,updated_at) VALUES(?,?,'native',?,?)", ownerID, subjectID, formatHubTime(now), formatHubTime(now))
					} else {
						err = changeRelatedIssuesTx(t.Context(), tx, subjectID, ownerID, "add", now)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "concurrent" {
				start := make(chan struct{})
				results := make(chan error, 2)
				for range 2 {
					go func() {
						<-start
						tx, err := f.service.database.db.BeginTx(t.Context(), nil)
						if err == nil {
							defer tx.Rollback()
							_, err = writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, findings)
							if err == nil {
								_, err = reportHealthFindings(t.Context(), tx, f.project.OrganizationID, now, nil)
							}
							if err == nil {
								err = tx.Commit()
							}
						}
						results <- err
					}()
				}
				close(start)
				var failures error
				for range 2 {
					failures = errors.Join(failures, <-results)
				}
				if failures != nil {
					t.Fatal(failures)
				}
			} else {
				applyTestHealth(t, f, now, findings)
				applyTestHealth(t, f, now.Add(time.Minute), findings)
			}
			for _, fixture := range []nativeFixture{f, other} {
				scope := healthIssueScope(f.project.OrganizationID, fixture.project.ID)
				var item tracker.NativeWorkItemID
				var count, comments int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT work_item_id FROM health_finding_issues WHERE project_id=?", fixture.project.ID).Scan(&item); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM issues WHERE project_id=?),(SELECT count(*) FROM native_comments WHERE project_id=?)", fixture.project.ID, fixture.project.ID).Scan(&count, &comments); err != nil {
					t.Fatal(err)
				}
				if test.state != "" {
					retained, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(owners[fixture.project.ID]))
					old := previous[fixture.project.ID]
					if err != nil || retained.Body != old.Body || retained.State != old.State || retained.Revision != old.Revision {
						t.Fatalf("terminal history changed: %+v, %v", retained, err)
					}
				}
				if test.state == "Cancelled" || test.held || test.instance != "" {
					if item != owners[fixture.project.ID] || count != 2 || comments != 0 {
						t.Fatalf("terminal decision overridden: item=%s issues=%d comments=%d", item, count, comments)
					}
					continue
				}
				openOwner := test.state == "" && owners[fixture.project.ID] != ""
				if openOwner {
					if item != owners[fixture.project.ID] || count != 2 || comments != 1 {
						t.Fatalf("existing owner not reused: item=%s owner=%s issues=%d comments=%d", item, owners[fixture.project.ID], count, comments)
					}
					var body string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT body FROM native_comments WHERE work_item_id=?", item).Scan(&body); err != nil {
						t.Fatal(err)
					}
					for _, evidence := range []string{"attempt-evidence", "event-evidence", `"recoveries": 5`, formatHubTime(now)} {
						if !strings.Contains(body, evidence) {
							t.Fatalf("occurrence missing %q: %s", evidence, body)
						}
					}
					continue
				}
				wantCount := 2
				if test.state == "Done" {
					wantCount++
				}
				if count != wantCount || comments != 0 || item == owners[fixture.project.ID] {
					t.Fatalf("new episode: issues=%d comments=%d item=%s", count, comments, item)
				}
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item))
				if err != nil {
					t.Fatal(err)
				}
				if issue.State != "Todo" || issue.Priority == nil || *issue.Priority != 1 || !slices.Equal(issue.Labels, []string{"infrastructure"}) || issue.Actor != scope.actor() {
					t.Fatalf("new finding routing = %+v", issue)
				}
				contract := config.IssueContract{Sections: []string{"Current behavior", "Desired behavior", "Acceptance criteria", "Must not break", "How we know it worked", "Invariants touched", "Safety regression"}, Context: "## Issue Contract\n- Acceptance criteria\n\nAgent-authored criteria do not require human confirmation.\n"}
				if evaluation := contract.Evaluate(nativeContractIssue(issue)); !evaluation.Satisfied() {
					t.Fatalf("missing finding contract: %+v", evaluation)
				}
				if evaluation, err := evaluateNativeIssueContract(t.Context(), f.service.database.db, scope, issue); err != nil || !evaluation.Allowed {
					t.Fatalf("finding admission = %+v, %v", evaluation, err)
				}
				origin, ok := issueorigin.Parse(issue.Body)
				if !ok || origin.Fingerprint == "" || origin.Instance != "health_detector" {
					t.Fatalf("finding origin = %+v", origin)
				}
			}
		})
	}
}

func TestHealthFindingReusesLinkedIssueWhileSearchRebuilds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		signal     string
		signatures []string
	}{
		{signal: "scheduler_loop_behind", signatures: []string{"first signature", "second signature", "third signature"}},
		{signal: "retry_storm", signatures: []string{"protocol failure", "protocol failure", "protocol failure"}},
	} {
		t.Run(test.signal, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-linked-rebuild")
			addHealthBacklog(t, f)
			anchor := f.create(t, "Search rebuild anchor")
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_work_search_dirty(issue_id,organization_id,project_id) SELECT id,organization_id,project_id FROM issues WHERE native_id=?`, anchor.WorkItemID); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
			finding := newHealthFinding(test.signal, "instance", "project", string(f.project.ID), "Instance condition.", "Inspect the instance.", []string{string(f.project.ID)}, healthEvidence{Signatures: []string{test.signatures[0]}})
			countIntake := func() int {
				t.Helper()
				var count int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=? AND title LIKE 'intake(instance): %'", f.project.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			for i, signature := range test.signatures {
				finding.Evidence.Signatures = []string{signature}
				applyTestHealth(t, f, now.Add(time.Duration(i)*time.Minute), []healthFinding{finding})
				if got := countIntake(); got != 1 {
					t.Fatalf("intake issues after observation %d = %d, want 1", i+1, got)
				}
			}
			applyTestHealth(t, f, now.Add(3*time.Minute), nil)
			applyTestHealth(t, f, now.Add(70*time.Minute), []healthFinding{finding})
			var episodes int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings WHERE fingerprint=?", finding.Fingerprint).Scan(&episodes); err != nil {
				t.Fatal(err)
			}
			if got := countIntake(); episodes != 2 || got != 1 {
				t.Fatalf("later episode: findings=%d intake issues=%d, want 2 findings and 1 issue", episodes, got)
			}
		})
	}
}
