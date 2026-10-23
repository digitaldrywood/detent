package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIntakeFixture(t *testing.T) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "health-intake")
	addHealthBacklog(t, f)
	states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true})
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Cancelled','Cancelled',1,0,?,?)", f.project.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHealthIntakeSignatures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		class      string
		signatures [][]string
		issues     int
		comments   int
	}{
		{"shared signature", "instance", [][]string{{"landing refused: base_protected"}, {"landing refused: base_protected"}}, 1, 1},
		{"different signatures", "instance", [][]string{{"landing refused: base_protected"}, {"protocol failure"}}, 2, 0},
		{"overlapping signatures", "instance", [][]string{{"protocol failure", "landing refused: base_protected"}, {"landing refused: base_protected"}}, 2, 1},
		{"human remains unfiled", "human", [][]string{{"waiting"}, {"waiting"}}, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := healthIntakeFixture(t)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "health-other-intake")
			addHealthBacklog(t, other)
			now := f.service.config.now()
			var findings []healthFinding
			for i, signatures := range test.signatures {
				for _, fixture := range []nativeFixture{f, other} {
					item := fixture.create(t, []string{"first-item", "second-item"}[i])
					finding := newHealthFinding("retry_storm", test.class, "work_item", string(item.WorkItemID), "Repeated failures.", "Inspect attempts.", []string{string(fixture.project.ID)}, healthEvidence{Signatures: signatures, AttemptIDs: []string{[]string{"attempt_one", "attempt_two"}[i]}, Counts: map[string]int{"attempts": 5}})
					findings = append(findings, finding)
				}
				applyTestHealth(t, f, now.Add(time.Duration(i)*time.Minute), findings)
			}
			applyTestHealth(t, f, now.Add(2*time.Minute), findings)
			var issues, comments int
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM issues WHERE project_id=? AND title LIKE 'intake(instance):%'), (SELECT count(*) FROM native_comments WHERE project_id=?)`, f.project.ID, f.project.ID).Scan(&issues, &comments); err != nil {
				t.Fatal(err)
			}
			if issues != test.issues || comments != test.comments {
				t.Fatalf("issues/comments = %d/%d, want %d/%d", issues, comments, test.issues, test.comments)
			}
			if test.issues == 0 {
				return
			}
			for _, fixture := range []nativeFixture{f, other} {
				scope := healthIssueScope(fixture.project.OrganizationID, fixture.project.ID)
				page, err := f.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
				if err != nil || len(page.Items) != 2 {
					t.Fatalf("findings = %+v, %v", page, err)
				}
				for _, finding := range page.Items {
					if len(finding.Issues) != len(finding.Evidence.Signatures) {
						t.Fatalf("signature links = %+v", finding)
					}
					for _, link := range finding.Issues {
						issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(link.WorkItemID))
						if err != nil {
							t.Fatal(err)
						}
						origin, ok := issueorigin.Parse(issue.Body)
						if !ok || origin.Fingerprint == finding.Fingerprint || origin.Instance != "health_detector" {
							t.Fatalf("signature origin = %+v", origin)
						}
						response := performHubAPIRequest(t, f.service, http.MethodGet, fixture.base+"/work-items?open=true&fingerprint="+origin.Fingerprint, fixture.token, nil)
						requireNativeStatus(t, response, http.StatusOK)
						var matches tracker.NativeIssuePage
						decodeHubResponse(t, response, &matches)
						if len(matches.Items) != 1 || matches.Items[0].WorkItemID != issue.WorkItemID {
							t.Fatalf("fingerprint lookup = %+v", matches)
						}
					}
				}
				if test.name == "overlapping signatures" {
					index := slices.IndexFunc(page.Items, func(finding healthFinding) bool { return len(finding.Issues) == 2 })
					visible := page.Items[index].Issues[0].WorkItemID
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET actor_json='{"kind":"human","principal_id":"reporter"}' WHERE native_id=?`, visible); err != nil {
						t.Fatal(err)
					}
					scope.credential.HostedRole = "reporter"
					scope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "reporter"}
					reporterPage, err := f.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
					if err != nil || len(reporterPage.Items) == 0 {
						t.Fatalf("reporter findings = %+v, %v", reporterPage, err)
					}
					for _, finding := range reporterPage.Items {
						if len(finding.Issues) != 1 || finding.Issues[0].WorkItemID != visible {
							t.Fatalf("reporter received hidden issue links: %+v", finding.Issues)
						}
					}
				}
			}
			if test.comments > 0 {
				var body string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT body FROM native_comments WHERE project_id=? ORDER BY sequence DESC LIMIT 1", other.project.ID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				for _, evidence := range []string{findings[len(findings)-1].Subject.ID, "attempt_two", `"attempts": 5`} {
					if !strings.Contains(body, evidence) {
						t.Fatalf("occurrence missing %q: %s", evidence, body)
					}
				}
			}
		})
	}
}

func TestHealthFindingOccurrenceDeduplication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, signal, change string
		legacy               bool
	}{
		{name: "rolling retry evidence", signal: "retry_storm"},
		{name: "rolling unsplit evidence", signal: "runner_setup_failure"},
		{name: "rolling legacy evidence", signal: "retry_storm", legacy: true},
		{name: "severity change", signal: "retry_storm", change: "severity"},
		{name: "new signature", signal: "runner_setup_failure", change: "signature"},
		{name: "new legacy signature", signal: "retry_storm", legacy: true, change: "signature"},
		{name: "signature set order", signal: "runner_setup_failure", change: "order"},
		{name: "resolved then recurred", signal: "retry_storm", change: "recurrence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := healthIntakeFixture(t)
			now := f.service.config.now()
			finding := newHealthFinding(test.signal, "instance", "runner", "runner-example", "Repeated failures.", "Inspect attempts.", []string{string(f.project.ID)}, healthEvidence{Signatures: []string{"protocol failure"}})
			if test.change == "order" {
				finding.Evidence.Signatures = []string{"protocol failure", "workspace failure"}
			}
			scope := healthIssueScope(f.project.OrganizationID, f.project.ID)
			intake := healthIssueFindings(finding)[0]
			if test.legacy {
				intake = finding
			}
			request, err := healthIssueRequest(intake)
			if err != nil {
				t.Fatal(err)
			}
			request.State = "Backlog"
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			owner, err := createNativeFixtureIssueTx(t.Context(), tx, scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			if test.legacy {
				if _, err := writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, []healthFinding{finding}); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_digest) SELECT id,?,?,'' FROM health_findings`, f.project.ID, owner.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			assertOccurrences := func(want int) {
				t.Helper()
				var issues, comments int
				if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM issues WHERE project_id=?), (SELECT count(*) FROM native_comments WHERE work_item_id=? AND body LIKE '## New machine occurrence%')`, f.project.ID, owner.WorkItemID).Scan(&issues, &comments); err != nil {
					t.Fatal(err)
				}
				if issues != 1 || comments != want {
					t.Fatalf("issues/occurrences = %d/%d, want 1/%d", issues, comments, want)
				}
			}
			for i := 0; i < 4; i++ {
				finding.Evidence.AttemptIDs = []string{[]string{"attempt-one", "attempt-two", "attempt-three", "attempt-four"}[i]}
				finding.Evidence.Counts = map[string]int{"failures_1h": 5 + i, "failures_6h": 10 + i}
				finding.Evidence.Values = map[string]float64{"retry_ratio_1h": 0.5 + float64(i)/10, "retry_ratio_6h": 0.6 + float64(i)/10}
				applyTestHealth(t, f, now.Add(time.Duration(i)*time.Minute), []healthFinding{finding})
				assertOccurrences(1)
				page, err := f.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("findings = %+v, %v", page, err)
				}
				if !reflect.DeepEqual(page.Items[0].Evidence, finding.Evidence) {
					t.Fatalf("stale current evidence: %+v, want %+v", page.Items[0].Evidence, finding.Evidence)
				}
			}
			want := 2
			switch test.change {
			case "severity":
				finding.Severity = "watch"
			case "signature":
				finding.Evidence.Signatures = append(finding.Evidence.Signatures, "workspace failure")
			case "order":
				finding.Evidence.Signatures = []string{"workspace failure", "protocol failure", "protocol failure"}
				want = 1
			case "recurrence":
				applyTestHealth(t, f, now.Add(4*time.Minute), nil)
			default:
				return
			}
			applyTestHealth(t, f, now.Add(5*time.Minute), []healthFinding{finding})
			assertOccurrences(want)
			applyTestHealth(t, f, now.Add(6*time.Minute), []healthFinding{finding})
			assertOccurrences(want)
		})
	}
}

func TestHealthIntakeResolution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		age        time.Duration
		unresolved bool
		move       bool
		returnMove bool
		legacy     bool
		recurrence string
		want       string
	}{
		{name: "all resolved", age: time.Hour + time.Minute, want: "Cancelled"},
		{name: "quiet window", age: time.Hour - time.Second, want: "Backlog"},
		{name: "exact window", age: time.Hour, want: "Backlog"},
		{name: "one unresolved", age: time.Hour + time.Minute, unresolved: true, want: "Backlog"},
		{name: "human Todo", age: time.Hour + time.Minute, move: true, want: "Todo"},
		{name: "human returned to Backlog", age: time.Hour + time.Minute, move: true, returnMove: true, want: "Backlog"},
		{name: "legacy link", age: time.Hour + time.Minute, legacy: true, want: "Cancelled"},
		{name: "new item after automatic cancellation", age: time.Hour + time.Minute, recurrence: "work_three", want: "Cancelled"},
		{name: "same item after automatic cancellation", age: time.Hour + time.Minute, recurrence: "work_one", want: "Cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := healthIntakeFixture(t)
			now := f.service.config.now()
			var findings []healthFinding
			for _, subject := range []string{"work_one", "work_two"} {
				findings = append(findings, newHealthFinding("retry_storm", "instance", "work_item", subject, "Repeated failure.", "Inspect attempts.", []string{string(f.project.ID)}, healthEvidence{Signatures: []string{"protocol failure"}}))
			}
			if test.legacy {
				findings = findings[:1]
				migration, err := migrationFiles.ReadFile("migrations/20261021140000_health_intake_signatures.sql")
				if err != nil {
					t.Fatal(err)
				}
				_, down, ok := strings.Cut(string(migration), "-- +goose Down")
				if !ok {
					t.Fatal("missing migration rollback")
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
					t.Fatal(err)
				}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, findings); err != nil {
					t.Fatal(err)
				}
				var id string
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM health_findings").Scan(&id); err != nil {
					t.Fatal(err)
				}
				body := issueorigin.Stamp("Legacy diagnostic\n<!-- detent:no-dispatch -->", issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: id, Fingerprint: findings[0].Fingerprint})
				issue, err := createNativeFixtureIssueTx(t.Context(), tx, healthIssueScope(f.project.OrganizationID, f.project.ID), tracker.CreateIssue{Title: "intake(instance): legacy", Body: body, State: "Backlog"}, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_json) SELECT id,?,?,evidence_json FROM health_findings", f.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hub_schema_version WHERE version_id=20261021140000"); err != nil {
					t.Fatal(err)
				}
				if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
					t.Fatal(err)
				}
				applyTestHealth(t, f, now.Add(30*time.Second), findings)
				var links int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_finding_issues").Scan(&links); err != nil || links != 1 {
					t.Fatalf("legacy observation duplicated its link: count=%d, %v", links, err)
				}
			} else {
				applyTestHealth(t, f, now, findings)
			}
			scope := healthIssueScope(f.project.OrganizationID, f.project.ID)
			var item string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT work_item_id FROM health_finding_issues LIMIT 1").Scan(&item); err != nil {
				t.Fatal(err)
			}
			if test.move {
				for _, state := range []string{"Todo", "Backlog"} {
					if state == "Backlog" && !test.returnMove {
						break
					}
					issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, item)
					if err != nil {
						t.Fatal(err)
					}
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+item+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: state}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			var active []healthFinding
			if test.unresolved {
				active = findings[1:]
			}
			resolvedAt := now.Add(time.Minute)
			applyTestHealth(t, f, resolvedAt, active)
			applyTestHealth(t, f, resolvedAt.Add(test.age), active)
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, item)
			if err != nil || issue.State != test.want {
				t.Fatalf("resolution state = %s, want %s: %v", issue.State, test.want, err)
			}
			if test.legacy {
				origin, ok := issueorigin.Parse(issue.Body)
				if !ok || origin.Fingerprint != findings[0].Fingerprint {
					t.Fatalf("legacy origin changed: %+v", origin)
				}
			}
			if test.want == "Cancelled" {
				var detail string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT json_extract(data_json,'$.reason_detail') FROM collaboration_events WHERE work_item_id=? AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1", item).Scan(&detail); err != nil {
					t.Fatal(err)
				}
				page, err := f.service.readHealthFindings(t.Context(), scope, "resolved", "", "", 100)
				if err != nil || len(page.Items) != len(findings) {
					t.Fatalf("resolved findings = %+v, %v", page, err)
				}
				for _, finding := range page.Items {
					if !strings.Contains(detail, finding.ID) {
						t.Fatalf("reason missing %s: %s", finding.ID, detail)
					}
				}
			}
			if test.recurrence != "" {
				previous := issue
				recurredAt := resolvedAt.Add(test.age + time.Minute)
				finding := newHealthFinding("retry_storm", "instance", "work_item", test.recurrence, "Repeated failure.", "Inspect attempts.", []string{string(f.project.ID)}, healthEvidence{Signatures: []string{"protocol failure"}, AttemptIDs: []string{"attempt_recurred"}})
				applyTestHealth(t, f, recurredAt, []healthFinding{finding})
				applyTestHealth(t, f, recurredAt.Add(time.Minute), []healthFinding{finding})
				origin, ok := issueorigin.Parse(previous.Body)
				if !ok {
					t.Fatal("missing previous origin")
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?open=true&fingerprint="+origin.Fingerprint, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var matches tracker.NativeIssuePage
				decodeHubResponse(t, response, &matches)
				if len(matches.Items) != 1 || matches.Items[0].WorkItemID == previous.WorkItemID || matches.Items[0].State != "Backlog" {
					t.Fatalf("new episode did not file one open intake: %+v", matches.Items)
				}
				current, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(previous.WorkItemID))
				if err != nil || current.State != previous.State || current.Revision != previous.Revision || current.Body != previous.Body {
					t.Fatalf("new episode changed terminal history: %+v, %v", current, err)
				}
				page, err := f.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
				if err != nil || len(page.Items) != 1 || len(page.Items[0].Issues) != 1 || page.Items[0].Issues[0].WorkItemID != matches.Items[0].WorkItemID {
					t.Fatalf("new episode link = %+v, %v", page, err)
				}
				followup := newHealthFinding(finding.Signal, finding.Class, finding.Subject.Kind, "work_followup", finding.Summary, finding.NextAction, finding.Projects, finding.Evidence)
				applyTestHealth(t, f, recurredAt.Add(2*time.Minute), []healthFinding{finding, followup})
				var comments int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", matches.Items[0].WorkItemID).Scan(&comments); err != nil || comments != 1 {
					t.Fatalf("new episode followup comments = %d, want 1: %v", comments, err)
				}
			}
		})
	}
}
