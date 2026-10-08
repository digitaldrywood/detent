package hubserver

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func addHealthBacklog(t *testing.T, f nativeFixture) {
	t.Helper()
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
	for i := range states {
		if states[i].Name == "Todo" {
			states[i].Transitions = append(states[i].Transitions, "Backlog")
		}
	}
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(raw), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Backlog','Backlog',0,0,?,?)", f.project.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFindingIssueLifecycle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, class string
		linked      bool
	}{
		{name: "instance", class: "instance"},
		{name: "flow", class: "flow"},
		{name: "capacity", class: "capacity"},
		{name: "cost", class: "cost"},
		{name: "human", class: "human"},
		{name: "flow with existing issue", class: "flow", linked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-issues")
			addHealthBacklog(t, f)
			original := f.create(t, "affected-item")
			now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
			finding := newHealthFinding("retry_storm", test.class, "work_item", string(original.WorkItemID), "Repeated failures.", "Inspect the protocol signature.", []string{string(f.project.ID)}, healthEvidence{AttemptIDs: []string{"attempt_one"}, Signatures: []string{"protocol failure"}, Counts: map[string]int{"attempts": 3}})
			var owner tracker.NativeIssue
			var firstFinding string
			if test.linked {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, []healthFinding{finding}); err != nil {
					t.Fatal(err)
				}
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM health_findings").Scan(&firstFinding); err != nil {
					t.Fatal(err)
				}
				body := issueorigin.Stamp("Legacy flow finding", issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: firstFinding, Fingerprint: finding.Fingerprint})
				owner, err = createNativeIssueTx(t.Context(), tx, healthIssueScope(f.project.OrganizationID, f.project.ID), tracker.CreateIssue{Title: "intake: legacy flow finding", Body: body, State: "Backlog"}, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_json) SELECT id,?,?,evidence_json FROM health_findings`, f.project.ID, owner.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			for _, step := range []struct {
				name                      string
				offset                    time.Duration
				active, changed, terminal bool
				comments                  int
			}{
				{name: "files", active: true},
				{name: "unchanged observation", offset: time.Minute, active: true},
				{name: "new evidence comments", offset: 2 * time.Minute, active: true, changed: true, comments: 1},
				{name: "resolves", offset: 3 * time.Minute, comments: 2},
				{name: "resolution does not repeat", offset: 4 * time.Minute, comments: 2},
				{name: "reopens finding and comments", offset: 5 * time.Minute, active: true, comments: 3},
				{name: "resolves again", offset: 6 * time.Minute, comments: 4},
				{name: "later finding reuses issue", offset: 67 * time.Minute, active: true, comments: 5},
				{name: "terminal issue stays handled", offset: 68 * time.Minute, active: true, changed: true, terminal: true, comments: 5},
				{name: "terminal resolution stays handled", offset: 69 * time.Minute, comments: 5},
			} {
				t.Run(step.name, func(t *testing.T) {
					if step.terminal && owner.WorkItemID != "" {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE native_id=?", f.project.ID, owner.WorkItemID); err != nil {
							t.Fatal(err)
						}
					}
					if step.changed {
						finding.Evidence.Counts["attempts"]++
						finding.Evidence.AttemptIDs = append(finding.Evidence.AttemptIDs, "attempt_new")
					}
					active, state := []healthFinding{}, "resolved"
					if step.active {
						active, state = []healthFinding{finding}, "open"
					}
					applyTestHealth(t, f, now.Add(step.offset), active)
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?state="+state, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var page healthFindingsPage
					decodeHubResponse(t, response, &page)
					if len(page.Items) == 0 {
						t.Fatal("missing finding")
					}
					read := page.Items[len(page.Items)-1]
					if read.Class != test.class || (read.ResolvedAt == nil) != step.active {
						t.Fatalf("finding state = %+v", read)
					}
					if test.class != "instance" {
						wantIssues, wantLinks := 1, 0
						if test.linked {
							wantIssues++
							if read.ID == firstFinding {
								wantLinks = 1
								if len(read.Issues) != 1 || read.Issues[0].WorkItemID != owner.WorkItemID {
									t.Fatalf("legacy link changed: %+v", read.Issues)
								}
							}
						}
						var issues, comments int
						if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM issues WHERE project_id=?), (SELECT count(*) FROM native_comments)`, f.project.ID).Scan(&issues, &comments); err != nil {
							t.Fatal(err)
						}
						if issues != wantIssues || comments != 0 || len(read.Issues) != wantLinks {
							t.Fatalf("non-instance reporting: issues=%d comments=%d links=%d want %d/0/%d", issues, comments, len(read.Issues), wantIssues, wantLinks)
						}
						return
					}
					if read.FiledBy != "health_detector" || len(read.Issues) != 1 || read.Issues[0].ProjectID != f.project.ID {
						t.Fatalf("finding issue link = %+v", read)
					}
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
					issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(read.Issues[0].WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					if owner.WorkItemID == "" {
						owner, firstFinding = issue, read.ID
						if issue.Priority == nil || *issue.Priority != 1 || !slices.Equal(issue.Labels, []string{"infrastructure"}) || !strings.HasPrefix(issue.Title, "fix(instance): retry_storm on work_item ") {
							t.Fatalf("instance routing = %+v", issue)
						}
						if issue.State != "Todo" || issue.Actor != (tracker.Actor{Kind: "integration", PrincipalID: "health_detector"}) {
							t.Fatalf("state/attribution = %+v", issue)
						}
						origin, ok := issueorigin.Parse(issue.Body)
						if !ok || origin.Fingerprint != read.Fingerprint || origin.Source != read.ID || origin.Instance != "health_detector" {
							t.Fatalf("origin = %+v", origin)
						}
						for _, text := range []string{read.ID, "Repeated failures.", "Inspect the protocol signature.", "attempt_one", "protocol failure", formatHubTime(now)} {
							if !strings.Contains(issue.Body, text) {
								t.Fatalf("missing evidence %q", text)
							}
						}
					}
					if issue.WorkItemID != owner.WorkItemID || issue.Body != owner.Body {
						t.Fatalf("occurrence replaced owner: %+v", issue)
					}
					if step.offset < 68*time.Minute {
						wantState := "Backlog"
						if step.active {
							wantState = "Todo"
						}
						if issue.State != wantState {
							t.Fatalf("operational state = %s, want %s", issue.State, wantState)
						}
						tx, err := f.service.database.db.BeginTx(t.Context(), nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &scope, AvailableAt: now.Add(step.offset)}, nil, nil, nil, nil, nil, nil, nil, nil)
						if err != nil {
							t.Fatal(err)
						}
						_, id, err := readNativeIssue(t.Context(), tx, scope, string(issue.WorkItemID))
						if err != nil {
							t.Fatal(err)
						}
						if err := tx.Rollback(); err != nil {
							t.Fatal(err)
						}
						if slices.Contains(ids, id) != step.active {
							t.Fatalf("operational candidate = %v, want %v", slices.Contains(ids, id), step.active)
						}
					}
					if step.offset >= 68*time.Minute && (!issue.Terminal || issue.State != "Done") {
						t.Fatalf("terminal issue reopened: %+v", issue)
					}
					if step.offset == 67*time.Minute && read.ID == firstFinding {
						t.Fatal("later recurrence did not open a new finding")
					}
					var issues, comments int
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&issues); err != nil {
						t.Fatal(err)
					}
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", owner.WorkItemID).Scan(&comments); err != nil {
						t.Fatal(err)
					}
					if issues != 2 || comments != step.comments {
						t.Fatalf("issues=%d comments=%d want 2/%d", issues, comments, step.comments)
					}
					if step.name == "resolves" {
						var body string
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT body FROM native_comments WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1", owner.WorkItemID).Scan(&body); err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(body, "Resolved: "+formatHubTime(now.Add(step.offset))) || !strings.Contains(body, read.ID) {
							t.Fatalf("resolution evidence = %s", body)
						}
					}
					affected, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(original.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					a, _ := json.Marshal(affected)
					b, _ := json.Marshal(original)
					if string(a) != string(b) {
						t.Fatal("finding changed the affected work item")
					}
				})
			}
		})
	}
}

func TestHealthFindingReporterPreservesHistory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		state                  string
		priority, wantPriority int
	}{
		{"Backlog", 3, 1},
		{"In Progress", 0, 0},
		{"Done", 3, 3},
	} {
		t.Run(test.state, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-history")
			addHealthBacklog(t, f)
			now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
			finding := newHealthFinding("runner_heartbeat_gap", "instance", "runner", "runner_subject", "Missing heartbeat.", "Inspect the runner.", []string{string(f.project.ID)}, healthEvidence{})
			body := issueorigin.Stamp("Human question and migration hold remain authoritative.", issueorigin.Origin{Kind: "audit", Instance: "external-board-audit", Source: "imported-occurrence", Fingerprint: finding.Fingerprint})
			provenance := &tracker.Provenance{Provider: "github", ExternalID: "old-report", AuthorID: "operator", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			owner, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "Imported report", Body: body, State: test.state, Priority: new(test.priority), Labels: []string{"migration-hold"}, Provenance: provenance}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,visibility,status,work_item_id,execution_json,created_at,updated_at) VALUES('health-held-conversation',?,?,?,'private','active',?,'{}',?,?)`, f.project.OrganizationID, f.project.ID, credential.ID, owner.WorkItemID, formatHubTime(now), formatHubTime(now)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO conversation_questions(id,conversation_id,status,created_at,updated_at) VALUES('health-held-question','health-held-conversation','pending',?,?)`, formatHubTime(now), formatHubTime(now)); err != nil {
				t.Fatal(err)
			}
			applyTestHealth(t, f, now.Add(time.Minute), []healthFinding{finding})
			got, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(owner.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != owner.Title || got.Body != owner.Body || got.State != owner.State || !slices.Equal(got.Labels, owner.Labels) || got.Priority == nil || *got.Priority != test.wantPriority || got.Provenance == nil || *got.Provenance != *owner.Provenance {
				t.Fatalf("report replaced imported history or hold: %+v", got)
			}
			var item tracker.NativeWorkItemID
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT work_item_id FROM health_finding_issues").Scan(&item); err != nil {
				t.Fatal(err)
			}
			if item != owner.WorkItemID {
				t.Fatal("report did not reuse imported owner")
			}
			var pending, issues, comments int
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM conversation_questions WHERE status='pending'),
 (SELECT count(*) FROM issues WHERE project_id=?),
 (SELECT count(*) FROM native_comments)`, f.project.ID).Scan(&pending, &issues, &comments); err != nil {
				t.Fatal(err)
			}
			wantComments := 1
			if test.state == "Done" {
				wantComments = 0
			}
			if pending != 1 || issues != 1 || comments != wantComments {
				t.Fatalf("pending=%d issues=%d comments=%d", pending, issues, comments)
			}
		})
	}
}

func TestHealthFindingResolutionOwnership(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		preserved bool
	}{
		{"reported resolution before dispatch", false},
		{"human question", true},
		{"handwritten goal", true},
		{"human recovery park", true},
		{"operator label", true},
		{"active lane", true},
		{"expired unreleased lease", true},
		{"running attempt", true},
		{"retained checkpoint", true},
		{"change version and review", true},
		{"untrusted origin", true},
		{"wrong finding identity", true},
		{"wrong tenant", true},
		{"later unresolved observation", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-resolution")
			addHealthBacklog(t, f)
			now := f.service.config.now()
			finding := newHealthFinding("scheduler_loop_behind", "instance", "project", string(f.project.ID), "Scheduler refresh is behind.", "Check scheduler refresh health.", []string{string(f.project.ID)}, healthEvidence{})
			applyTestHealth(t, f, now, []healthFinding{finding})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			var item tracker.NativeWorkItemID
			var findingID string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT finding_id,work_item_id FROM health_finding_issues").Scan(&findingID, &item); err != nil {
				t.Fatal(err)
			}
			issue, id, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item))
			if err != nil {
				t.Fatal(err)
			}
			var checkpoint, version, review string
			switch test.name {
			case "human question":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "hold"}, Body: "Keep this open until I verify the scheduler."}), http.StatusOK)
			case "human recovery park":
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				runnerScope := scope
				runnerScope.sourceActor = &tracker.Actor{Kind: "runner", PrincipalID: "health-runner"}
				for _, body := range []string{"## Detent recovery park\n\n```detent-park\n{\"schema\":1,\"owner\":\"human\",\"phase\":\"applied\"}\n```", "Runner observed the scheduler again."} {
					if _, err := insertNativeComment(t.Context(), tx, runnerScope, issue, body, nil, now); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			case "handwritten goal":
				body := issue.Body + "\n\nAlso repair the scheduler source and verify deployment."
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "goal"}, ExpectedRevision: issue.Revision, Body: &body}), http.StatusOK)
			case "operator label":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET labels_json='["infrastructure","migration-hold"]' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "active lane":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE id=?`, scope.project, id); err != nil {
					t.Fatal(err)
				}
			case "expired unreleased lease", "running attempt", "retained checkpoint":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET issue_contract_json='{"exempt":true}' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker := f.worker(t, "health-worker")
				lease := claimNativeAttempt(t, f, worker, "health-machine", "health-session", item)
				if test.name == "expired unreleased lease" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now.Add(-time.Minute)), lease.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					start := nativeStartedEvent(lease)
					path := f.base + "/work-items/" + string(item) + "/events"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, start), http.StatusOK)
					if test.name == "retained checkpoint" {
						event := start
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", "checkpoint", 2, nativeTestCheckpoint()
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "run.finished", "finish", 3, "failed", nil
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "failed"}), http.StatusNoContent)
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT checkpoint_json FROM native_attempts WHERE id=?", start.Data.AttemptID).Scan(&checkpoint); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "change version and review":
				change := tracker.ChangeRequest{ID: "health-change", OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, CurrentVersion: "health-version"}
				raw, err := json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,?)", change.ID, scope.organization, scope.project, item, string(raw)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_issue_links(change_id,organization_id,project_id,work_item_id) VALUES(?,?,?,?)", change.ID, scope.organization, scope.project, item); err != nil {
					t.Fatal(err)
				}
				version = `{"id":"health-version","head_sha":"retained-head"}`
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES('health-version',?,1,?)", change.ID, version); err != nil {
					t.Fatal(err)
				}
				review = `{"review":"approved"}`
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,'health-version','review',?)", change.ID, review); err != nil {
					t.Fatal(err)
				}
			case "untrusted origin":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET actor_json='{"kind":"human","principal_id":"operator"}' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "wrong finding identity":
				finding.Fingerprint = "different-fingerprint"
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE health_findings SET fingerprint=? WHERE id=?", finding.Fingerprint, findingID); err != nil {
					t.Fatal(err)
				}
			case "wrong tenant":
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,local,created_at) VALUES('org_health_other','Other',0,?)", formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE health_findings SET organization_id='org_health_other' WHERE id=?", findingID); err != nil {
					t.Fatal(err)
				}
			}
			before, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := writeHealthEvaluation(t.Context(), tx, scope.organization, now.Add(time.Minute), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `UPDATE health_finding_issues SET reported_resolved_at=(SELECT resolved_at FROM health_findings WHERE id=finding_id)`); err != nil {
				t.Fatal(err)
			}
			if test.name == "later unresolved observation" {
				if _, err := writeHealthEvaluation(t.Context(), tx, scope.organization, now.Add(62*time.Minute), []healthFinding{finding}); err != nil {
					t.Fatal(err)
				}
				if _, err := reportHealthFindings(t.Context(), tx, scope.organization, now.Add(62*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &scope, AvailableAt: now.Add(63 * time.Minute)}, nil, nil, nil, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := readNativeIssue(t.Context(), tx, scope, string(item))
			if err != nil {
				t.Fatal(err)
			}
			if test.preserved {
				if got.State != before.State || got.Revision != before.Revision || got.Body != before.Body {
					t.Fatalf("protected issue changed: %+v", got)
				}
			} else if got.State != "Backlog" || got.Revision != before.Revision+1 || slices.Contains(ids, id) {
				t.Fatalf("resolved operational issue remains dispatchable: %+v, candidates=%v", got, ids)
			}
			if checkpoint != "" {
				var retained string
				if err := tx.QueryRowContext(t.Context(), "SELECT checkpoint_json FROM native_attempts WHERE work_item_id=?", item).Scan(&retained); err != nil {
					t.Fatal(err)
				}
				if retained != checkpoint {
					t.Fatal("checkpoint changed")
				}
			}
			if version != "" {
				var retainedVersion, retainedReview string
				if err := tx.QueryRowContext(t.Context(), "SELECT v.record_json,e.record_json FROM change_versions v JOIN change_evidence e ON e.version_id=v.id WHERE v.id='health-version'").Scan(&retainedVersion, &retainedReview); err != nil {
					t.Fatal(err)
				}
				if retainedVersion != version || retainedReview != review {
					t.Fatal("immutable source or review changed")
				}
			}
			var fabricated int
			if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM change_requests WHERE work_item_id=?", item).Scan(&fabricated); err != nil {
				t.Fatal(err)
			}
			if version == "" && fabricated != 0 {
				t.Fatal("operational acceptance fabricated a Change")
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			applyTestHealth(t, f, now.Add(64*time.Minute), []healthFinding{finding})
			recurred, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(item))
			if err != nil {
				t.Fatal(err)
			}
			if test.preserved && (recurred.State != before.State || recurred.Body != before.Body) {
				t.Fatal("recurrence erased a protected decision")
			}
			if !test.preserved && recurred.State != "Todo" {
				t.Fatal("new observation did not re-admit operational issue")
			}
		})
	}
}

func TestHealthFindingIssueScope(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-shared-runner")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "health-shared-other")
	now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
	finding := newHealthFinding("runner_heartbeat_gap", "instance", "runner", "shared_runner", "Missing heartbeat.", "Inspect runner.", []string{string(f.project.ID), string(other.project.ID)}, healthEvidence{Counts: map[string]int{"queue_depth": 12}, Queues: map[string]healthQueueEvidence{string(f.project.ID): {QueueDepth: 5}, string(other.project.ID): {QueueDepth: 7}}})
	applyTestHealth(t, f, now, []healthFinding{finding})
	for _, test := range []struct {
		fixture nativeFixture
		hidden  tracker.ProjectID
		depth   int
	}{
		{f, other.project.ID, 5},
		{other, f.project.ID, 7},
	} {
		scope := nativeScope{organization: test.fixture.project.OrganizationID, project: test.fixture.project.ID}
		page, err := f.service.readHealthFindings(t.Context(), scope, "", "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || len(page.Items[0].Issues) != 1 || page.Items[0].Issues[0].ProjectID != scope.project || len(page.Items[0].Evidence.Queues) != 1 || page.Items[0].Evidence.Counts["queue_depth"] != test.depth {
			t.Fatalf("finding escaped scope: %+v", page)
		}
		issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(page.Items[0].Issues[0].WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(issue.Body, string(test.hidden)) {
			t.Fatal("issue evidence escaped scope")
		}
	}
}

func TestHealthFindingIssueRollback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, table string
		existing    bool
	}{
		{"filing fails", "health_finding_issues", false},
		{"occurrence fails", "native_comments", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-rollback")
			now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
			finding := newHealthFinding("runner_heartbeat_gap", "instance", "runner", "runner", "Missing heartbeat.", "Inspect runner.", []string{string(f.project.ID)}, healthEvidence{Counts: map[string]int{"leases": 1}})
			if test.existing {
				applyTestHealth(t, f, now, []healthFinding{finding})
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "CREATE TRIGGER fail_health_reporting BEFORE INSERT ON "+test.table+" BEGIN SELECT RAISE(ABORT,'fixture reporting failure'); END"); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			finding.Evidence.Counts["leases"] = 2
			err = f.service.commitHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now.Add(time.Minute), []healthFinding{finding})
			if err == nil || !strings.Contains(err.Error(), "fixture reporting failure") {
				t.Fatalf("report error = %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.existing {
				want = 1
			}
			for _, table := range []string{"issues", "health_findings", "health_finding_issues", "health_detector_ticks", "native_comments"} {
				var count int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				wantCount := want
				if table == "native_comments" {
					wantCount = 0
				}
				if count != wantCount {
					t.Fatalf("%s=%d want %d", table, count, wantCount)
				}
			}
			if test.existing {
				var seen, tick string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT f.last_seen_at,t.last_tick_at FROM health_findings f JOIN health_detector_ticks t ON t.organization_id=f.organization_id").Scan(&seen, &tick); err != nil {
					t.Fatal(err)
				}
				if seen != formatHubTime(now) || tick != formatHubTime(now) {
					t.Fatal("failed reporting advanced the finding or tick")
				}
			}
		})
	}
}
