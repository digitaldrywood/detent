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
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog"})
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
	for _, class := range []string{"instance", "flow", "capacity", "cost", "human"} {
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-issues")
			addHealthBacklog(t, f)
			original := f.create(t, "affected-item")
			now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
			finding := newHealthFinding("retry_storm", class, "work_item", string(original.WorkItemID), "Repeated failures.", "Inspect the protocol signature.", []string{string(f.project.ID)}, healthEvidence{AttemptIDs: []string{"attempt_one"}, Signatures: []string{"protocol failure"}, Counts: map[string]int{"attempts": 3}})
			var owner tracker.NativeIssue
			var firstFinding string
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
					if step.terminal {
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
						wantState := "Backlog"
						if class == "instance" {
							wantState = "Todo"
							if issue.Priority == nil || *issue.Priority != 1 || !slices.Equal(issue.Labels, []string{"infrastructure"}) || !strings.HasPrefix(issue.Title, "fix(instance): retry_storm on work_item ") {
								t.Fatalf("instance routing = %+v", issue)
							}
						} else if issue.Priority != nil || len(issue.Labels) != 0 {
							t.Fatalf("intake priority/labels = %+v", issue)
						}
						if issue.State != wantState || issue.Actor != (tracker.Actor{Kind: "integration", PrincipalID: "health_detector"}) {
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

func TestHealthFindingIssueScope(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-shared-runner")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "health-shared-other")
	for _, project := range []nativeFixture{f, other} {
		addHealthBacklog(t, project)
	}
	now := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
	finding := newHealthFinding("capacity_starvation", "capacity", "runner", "shared_runner", "Runner is full.", "Inspect slots.", []string{string(f.project.ID), string(other.project.ID)}, healthEvidence{Counts: map[string]int{"queue_depth": 12}, Queues: map[string]healthQueueEvidence{string(f.project.ID): {QueueDepth: 5}, string(other.project.ID): {QueueDepth: 7}}})
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
