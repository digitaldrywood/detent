package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func testDashboardChangeLog(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{Workspace: &WorkspaceConfig{Enabled: true}})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "dashboard-other")
	fixtures := []nativeFixture{f, other}
	issues := []tracker.NativeIssue{f.create(t, "First dashboard item"), other.create(t, "Second dashboard item")}
	d := f.service.database
	now := f.service.config.now()
	scopes := make([]nativeScope, len(fixtures))
	for i, fixture := range fixtures {
		scopes[i] = nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	}
	exec := func(tx *sql.Tx, statement string, args ...any) error {
		_, err := tx.ExecContext(t.Context(), statement, args...)
		return err
	}
	seed, err := d.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback()
	for i, scope := range scopes {
		id := strconv.Itoa(i)
		if err := insertWorkspace(t.Context(), seed, workspaceRecord{ID: "ws_dashboard_" + id, OrganizationID: scope.organization, ProjectID: scope.project, SubjectWorkItemID: string(issues[i].WorkItemID), Ref: "develop", State: "requested", IdleTimeoutSeconds: 60, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: "example-owner", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := insertProjectAction(t.Context(), seed, actionRecord{ID: "action_dashboard_" + id, OrganizationID: scope.organization, ProjectID: scope.project, Name: "Example action", Command: "true", Icon: "play", CreatedBy: "example-owner", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := insertProjectActionRun(t.Context(), seed, actionRunRecord{ID: "run_dashboard_" + id, OrganizationID: scope.organization, ProjectID: scope.project, ActionID: "action_dashboard_" + id, WorkspaceID: "ws_dashboard_" + id, Command: "true", Status: "queued", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec(seed, `INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json)
VALUES('finding_dashboard',?,'dashboard','example','flow','{}','[]',?,?,'attention','Example finding','Inspect','{}')`, f.project.OrganizationID, formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := syncHealthFindingProjects(t.Context(), seed, f.project.OrganizationID, "finding_dashboard", []string{string(f.project.ID), string(other.project.ID)}); err != nil {
		t.Fatal(err)
	}
	if err := appendDashboardHealthFinding(t.Context(), seed, "finding_dashboard"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	head := func(scope nativeScope) int64 {
		t.Helper()
		value, _, err := readDashboardWatermark(t.Context(), d.reader, scope)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	count := func(q nativeQueryer, scope nativeScope, since int64, topic, entity string) int {
		t.Helper()
		var value int
		if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_deltas WHERE organization_id=? AND project_id=? AND sequence>? AND topic=? AND entity_id=?", scope.organization, scope.project, since, topic, entity).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("source creation produces one revision per project", func(t *testing.T) {
		for i, scope := range scopes {
			for _, test := range []struct{ topic, entity string }{
				{"work_item", string(issues[i].WorkItemID)},
				{"human_action", string(issues[i].WorkItemID)},
				{"workspace", "ws_dashboard_" + strconv.Itoa(i)},
				{"action_run", "run_dashboard_" + strconv.Itoa(i)},
				{"health", "finding_dashboard"},
			} {
				if got := count(d.reader, scope, 0, test.topic, test.entity); got != 1 {
					t.Fatalf("project %d %s creation rows=%d", i, test.topic, got)
				}
			}
		}
	})
	t.Run("source revisions commit once and roll back together", func(t *testing.T) {
		for i, scope := range scopes {
			id := strconv.Itoa(i)
			var laneID int64
			if err := d.reader.QueryRowContext(t.Context(), "SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Todo'", scope.project).Scan(&laneID); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				topic, entity string
				revision      int64
				mutate        func(*sql.Tx) error
			}{
				{"work_item", string(issues[i].WorkItemID), 10, func(tx *sql.Tx) error {
					return exec(tx, "UPDATE issues SET revision=10,title='Updated dashboard item' WHERE native_id=?", issues[i].WorkItemID)
				}},
				{"human_action", string(issues[i].WorkItemID), 11, func(tx *sql.Tx) error {
					return exec(tx, "UPDATE issues SET revision=11,issue_contract_json=? WHERE native_id=?", `{"human_action":"Choose an option"}`, issues[i].WorkItemID)
				}},
				{"lane", "Todo", -1, func(tx *sql.Tx) error {
					return exec(tx, "UPDATE workflow_states SET updated_at=? WHERE id=?", formatHubTime(now.Add(time.Second)), laneID)
				}},
				{"activity", "", 0, func(tx *sql.Tx) error {
					return appendLegacyNativeHistory(t.Context(), tx, scope, string(issues[i].WorkItemID), "scheduler.decision", tracker.CollaborationData{}, now)
				}},
				{"workspace", "ws_dashboard_" + id, 10, func(tx *sql.Tx) error {
					return exec(tx, "UPDATE workspace_sessions SET revision=10,state='ready' WHERE id=?", "ws_dashboard_"+id)
				}},
				{"action_run", "run_dashboard_" + id, 10, func(tx *sql.Tx) error {
					return exec(tx, "UPDATE project_action_runs SET revision=10,status='running' WHERE id=?", "run_dashboard_"+id)
				}},
			} {
				t.Run(id+"/"+test.topic, func(t *testing.T) {
					before := head(scope)
					for _, commit := range []bool{false, true} {
						tx, err := d.db.BeginTx(t.Context(), nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if err := test.mutate(tx); err != nil {
							t.Fatal(err)
						}
						entity, revision := test.entity, test.revision
						if test.topic == "activity" {
							if err := tx.QueryRowContext(t.Context(), "SELECT id,sequence FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1", issues[i].WorkItemID).Scan(&entity, &revision); err != nil {
								t.Fatal(err)
							}
						}
						if got := count(tx, scope, before, test.topic, entity); got != 1 {
							t.Fatalf("transaction has %d feed rows, want one", got)
						}
						if test.topic == "lane" {
							if err := tx.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_deltas WHERE project_id=? AND topic='lane' AND entity_id=? AND sequence>?", scope.project, entity, before).Scan(&revision); err != nil {
								t.Fatal(err)
							}
							if revision <= before {
								t.Fatal("lane revision did not advance")
							}
						}
						var gotRevision int64
						if err := tx.QueryRowContext(t.Context(), "SELECT entity_revision FROM native_board_deltas WHERE project_id=? AND topic=? AND entity_id=? AND sequence>?", scope.project, test.topic, entity, before).Scan(&gotRevision); err != nil || gotRevision != revision {
							t.Fatalf("revision=%d want %d: %v", gotRevision, revision, err)
						}
						if got := head(scope); got != before {
							t.Fatalf("uncommitted head=%d want %d", got, before)
						}
						if commit {
							if err := tx.Commit(); err != nil {
								t.Fatal(err)
							}
							if got := count(d.reader, scope, before, test.topic, entity); got != 1 {
								t.Fatalf("committed rows=%d", got)
							}
						} else {
							if err := tx.Rollback(); err != nil {
								t.Fatal(err)
							}
							if got := count(d.reader, scope, before, test.topic, entity); got != 0 {
								t.Fatalf("rolled-back rows=%d", got)
							}
						}
					}
				})
			}
		}
	})
	t.Run("health projects and revisions are atomic", func(t *testing.T) {
		before := []int64{head(scopes[0]), head(scopes[1])}
		for _, commit := range []bool{false, true} {
			tx, err := d.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := exec(tx, "UPDATE health_findings SET severity='watch' WHERE id='finding_dashboard'"); err != nil {
				t.Fatal(err)
			}
			for i, scope := range scopes {
				if got := count(tx, scope, before[i], "health", "finding_dashboard"); got != 1 {
					t.Fatalf("project %d health rows=%d", i, got)
				}
				if head(scope) != before[i] {
					t.Fatal("health head escaped transaction")
				}
			}
			if commit {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, scope := range scopes {
				want := 0
				if commit {
					want = 1
				}
				if got := count(d.reader, scope, before[i], "health", "finding_dashboard"); got != want {
					t.Fatalf("health rows=%d want %d", got, want)
				}
			}
		}
	})
	t.Run("health writer routes membership changes once per revision", func(t *testing.T) {
		finding := hubhealth.Finding{Fingerprint: "dashboard-writer", Signal: "example", Class: "flow", Severity: "watch", Summary: "Example finding", NextAction: "Inspect", Projects: []string{string(f.project.ID), string(other.project.ID)}}
		var findingID string
		for step, test := range []struct {
			projects []string
			kinds    [2]string
			resolved bool
		}{
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{[]string{string(other.project.ID)}, [2]string{"delete", "upsert"}, false},
			{[]string{}, [2]string{"", "delete"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, true},
		} {
			before := []int64{head(scopes[0]), head(scopes[1])}
			tx, err := d.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			findings := []hubhealth.Finding{finding}
			findings[0].Projects = test.projects
			if test.resolved {
				findings = nil
			}
			if _, err := writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now.Add(time.Duration(step)*time.Second), findings); err != nil {
				t.Fatal(err)
			}
			if step == 0 {
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM health_findings WHERE fingerprint='dashboard-writer'").Scan(&findingID); err != nil {
					t.Fatal(err)
				}
			}
			for i, scope := range scopes {
				want := 1
				if test.kinds[i] == "" {
					want = 0
				}
				if got := count(tx, scope, before[i], "health", findingID); got != want {
					t.Fatalf("step %d project %d rows=%d want %d", step, i, got, want)
				}
				if want == 1 {
					var kind, status string
					var revision int64
					if err := tx.QueryRowContext(t.Context(), "SELECT kind,status,entity_revision FROM native_board_deltas WHERE project_id=? AND topic='health' AND entity_id=? AND sequence>?", scope.project, findingID, before[i]).Scan(&kind, &status, &revision); err != nil {
						t.Fatal(err)
					}
					if kind != test.kinds[i] || revision != int64(step+1) || test.resolved && status != "resolved" {
						t.Fatalf("step %d project %d change=%s/%s/%d", step, i, kind, status, revision)
					}
				}
				if head(scope) != before[i] {
					t.Fatal("health writer exposed an uncommitted event")
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("source snapshot cannot suppress a later projection", func(t *testing.T) {
		scope := scopes[0]
		page, err := f.service.readIssues(t.Context(), scope, url.Values{"include": {"board"}})
		if err != nil {
			t.Fatal(err)
		}
		if page.ChangeSequence != head(scope) {
			t.Fatalf("board watermark=%d head=%d", page.ChangeSequence, head(scope))
		}
		before := page.ChangeSequence
		tx, err := d.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := refreshNativeBoardCard(t.Context(), tx, string(issues[0].WorkItemID), now, map[string]changeSummaryPolicy{}); err != nil {
			t.Fatal(err)
		}
		if count(tx, scope, before, "board", string(issues[0].WorkItemID)) != 1 || count(d.reader, scope, before, "board", string(issues[0].WorkItemID)) != 0 {
			t.Fatal("projection feed escaped its transaction")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if head(scope) != before {
			t.Fatal("projection rollback advanced head")
		}
		drainTestNativeBoard(t, f.service)
		frame, err := readNativeBoardFrame(t.Context(), d.reader, scope, before)
		if err != nil || frame.Gap || len(frame.Deltas) != 1 || frame.Deltas[0].Current.Issue.Title != "Updated dashboard item" || frame.Deltas[0].Sequence <= before {
			t.Fatalf("later projection=%#v: %v", frame, err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issues[0].WorkItemID), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot tracker.NativeIssue
		decodeHubResponse(t, response, &snapshot)
		if snapshot.ChangeSequence != head(scope) || snapshot.Revision != 11 {
			t.Fatalf("snapshot=%#v", snapshot)
		}
	})
	t.Run("serialized commits and read snapshots", func(t *testing.T) {
		first, err := d.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Rollback()
		var globalBefore int64
		if err := first.QueryRowContext(t.Context(), "SELECT COALESCE(MAX(sequence),0) FROM native_board_deltas").Scan(&globalBefore); err != nil {
			t.Fatal(err)
		}
		before := head(scopes[0])
		if err := exec(first, "UPDATE issues SET revision=12,title='First writer' WHERE native_id=?", issues[0].WorkItemID); err != nil {
			t.Fatal(err)
		}
		var firstSequence int64
		if err := first.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_sequences WHERE project_id=?", scopes[0].project).Scan(&firstSequence); err != nil {
			t.Fatal(err)
		}
		started, written, release, done := make(chan struct{}), make(chan error, 1), make(chan struct{}), make(chan error, 1)
		finished := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
			_ = first.Rollback()
			<-finished
		})
		go func() {
			defer close(finished)
			close(started)
			tx, err := d.db.BeginTx(t.Context(), nil)
			if err != nil {
				written <- err
				done <- err
				return
			}
			defer tx.Rollback()
			_, err = tx.ExecContext(t.Context(), "UPDATE issues SET revision=13,title='Second writer' WHERE native_id=?", issues[1].WorkItemID)
			written <- err
			if err == nil {
				<-release
				err = tx.Commit()
			}
			done <- err
		}()
		<-started
		if head(scopes[0]) != before {
			t.Fatal("uncommitted writer became visible")
		}
		if err := first.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		snapshot, err := d.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer snapshot.Rollback()
		watermark, _, err := readDashboardWatermark(t.Context(), snapshot, scopes[0])
		if err != nil || watermark != firstSequence {
			t.Fatalf("visible watermark=%d want %d: %v", watermark, firstSequence, err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		old, _, err := readNativeIssue(t.Context(), snapshot, scopes[1], string(issues[1].WorkItemID))
		if err != nil || old.Revision != 11 || old.Title != "Updated dashboard item" {
			t.Fatalf("read snapshot=%#v: %v", old, err)
		}
		if got := head(scopes[1]); got <= firstSequence {
			t.Fatalf("second writer head=%d first=%d", got, firstSequence)
		}
		var revisions []int64
		for _, scope := range scopes {
			changes, err := readDashboardChanges(t.Context(), d.reader, scope, globalBefore, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range changes.Changes {
				if change.Topic == "work_item" {
					revisions = append(revisions, change.EntityRevision)
				}
			}
		}
		if fmt.Sprint(revisions) != "[12 13]" {
			t.Fatalf("commit order=%v", revisions)
		}
	})
	t.Run("dashboard snapshot pins its data and watermark", func(t *testing.T) {
		before := head(scopes[0])
		arrived, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var pause, resume sync.Once
		t.Cleanup(func() { resume.Do(func() { close(release) }); <-finished })
		ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "SELECT sequence,replay_floor FROM native_board_sequences") {
				pause.Do(func() { close(arrived); <-release })
			}
		}})
		type result struct {
			page tracker.NativeIssuePage
			err  error
		}
		results := make(chan result, 1)
		go func() {
			defer close(finished)
			page, err := f.service.readIssues(ctx, scopes[0], url.Values{})
			results <- result{page, err}
		}()
		select {
		case <-arrived:
		case result := <-results:
			t.Fatalf("snapshot did not reach its data read: %v", result.err)
		}
		if _, err := d.db.ExecContext(t.Context(), "UPDATE issues SET revision=14,title='After snapshot',archived=1 WHERE native_id=?", issues[0].WorkItemID); err != nil {
			t.Fatal(err)
		}
		resume.Do(func() { close(release) })
		got := <-results
		if got.err != nil || got.page.ChangeSequence != before || len(got.page.Items) != 1 || got.page.Items[0].Revision != 12 || got.page.Items[0].Title != "First writer" {
			t.Fatalf("snapshot mixed source revisions: page=%#v error=%v", got.page, got.err)
		}
		if head(scopes[0]) <= got.page.ChangeSequence {
			t.Fatal("concurrent write did not advance the durable head")
		}
	})
	t.Run("dashboard surface watermarks share their source snapshot", func(t *testing.T) {
		for i, scope := range scopes {
			workspaceJSON, err := f.service.readWorkspaces(t.Context(), scope, "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			var workspaces workspaceListResponse
			if err := json.Unmarshal(workspaceJSON, &workspaces); err != nil {
				t.Fatal(err)
			}
			if workspaces.ChangeSequence != head(scope) || len(workspaces.Workspaces) != 1 || workspaces.Workspaces[0].Revision != 10 {
				t.Fatalf("workspace snapshot=%#v", workspaces)
			}
			runJSON, err := f.service.readActionRuns(t.Context(), scope, "action_dashboard_"+strconv.Itoa(i), 100)
			if err != nil {
				t.Fatal(err)
			}
			var runs projectActionRunList
			if err := json.Unmarshal(runJSON, &runs); err != nil {
				t.Fatal(err)
			}
			if runs.ChangeSequence != head(scope) || len(runs.Items) != 1 || runs.Items[0].Revision != 10 {
				t.Fatalf("action snapshot=%#v", runs)
			}
			health, err := f.service.readHealthFindings(t.Context(), scope, "all", "", "", 100)
			if err != nil || health.ChangeSequence != head(scope) {
				t.Fatalf("health snapshot=%#v: %v", health, err)
			}
			runtime, err := f.service.readNativeRuntime(t.Context(), scope, string(issues[i].WorkItemID), "")
			if err != nil || runtime.ChangeSequence != head(scope) {
				t.Fatalf("runtime watermark=%d: %v", runtime.ChangeSequence, err)
			}
		}
	})
	t.Run("human attempt dispositions replay idempotently", func(t *testing.T) {
		for i, fixture := range fixtures {
			approveHubTestPolicy(t, fixture.service, fixture.base+"/policy", hubTestPolicy())
			issue := fixture.create(t, "Human question source")
			worker := fixture.worker(t, "dashboard-human-"+strconv.Itoa(i))
			lease := claimNativeAttempt(t, fixture, worker, "example-machine-"+strconv.Itoa(i), "example-session-"+strconv.Itoa(i), issue.WorkItemID)
			path := fixture.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			event := nativeStartedEvent(lease)
			event.IdempotencyKey = "dashboard-human-start"
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path, worker, event), http.StatusOK)
			before := head(scopes[i])
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "dashboard-human-finish", 2, "succeeded"
			event.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
			for range 2 {
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path, worker, event), http.StatusOK)
			}
			if got := count(d.reader, scopes[i], before, "human_action", event.Data.AttemptID); got != 1 {
				t.Fatalf("attempt disposition produced %d rows", got)
			}
			var revision int64
			var status, related string
			if err := d.reader.QueryRowContext(t.Context(), "SELECT entity_revision,status,related_id FROM native_board_deltas WHERE project_id=? AND topic='human_action' AND entity_id=?", scopes[i].project, event.Data.AttemptID).Scan(&revision, &status, &related); err != nil || revision != 2 || status != "blocked" || related != string(issue.WorkItemID) {
				t.Fatalf("human disposition=%d/%s/%s: %v", revision, status, related, err)
			}
		}
	})
	t.Run("deletion commits its tombstone", func(t *testing.T) {
		for i, scope := range scopes {
			before := head(scope)
			id := "run_dashboard_" + strconv.Itoa(i)
			for _, commit := range []bool{false, true} {
				tx, err := d.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := exec(tx, "DELETE FROM project_action_runs WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
				if count(tx, scope, before, "action_run", id) != 1 || count(d.reader, scope, before, "action_run", id) != 0 {
					t.Fatal("tombstone is not transactional")
				}
				if commit {
					err = tx.Commit()
				} else {
					err = tx.Rollback()
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			changes, err := readDashboardChanges(t.Context(), d.reader, scope, before, 100)
			if err != nil || len(changes.Changes) != 1 || changes.Changes[0].Kind != "delete" || changes.Changes[0].EntityID != id {
				t.Fatalf("tombstone=%#v: %v", changes, err)
			}
			var exists int
			if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM project_action_runs WHERE id=?", id).Scan(&exists); err != nil || exists != 0 {
				t.Fatalf("deleted row=%d: %v", exists, err)
			}
		}
	})
	t.Run("retention progresses on unchanged projects with an inclusive floor", func(t *testing.T) {
		plan := strings.Join(explainQueryPlan(t, d.reader, nativeBoardRetentionCandidateQuery, []any{"", ""}), "\n")
		if !strings.Contains(plan, "native_board_retention_due") || strings.Contains(plan, "USE TEMP B-TREE") {
			t.Fatalf("retention discovery is not indexed: %s", plan)
		}
		drainTestNativeBoard(t, f.service)
		for _, scope := range scopes {
			if _, err := d.db.ExecContext(t.Context(), `WITH RECURSIVE retained(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM retained WHERE n<?)
INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision) SELECT ?,?,'activity','retained-'||n,1 FROM retained`, nativeBoardReplayLimit+2*nativeBoardTrimLimit, scope.organization, scope.project); err != nil {
				t.Fatal(err)
			}
		}
		for _, scope := range scopes {
			var beforeCount, beforeHead, floor int64
			if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*),MAX(sequence) FROM native_board_deltas WHERE project_id=?", scope.project).Scan(&beforeCount, &beforeHead); err != nil {
				t.Fatal(err)
			}
			if err := d.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_deltas WHERE project_id=? ORDER BY sequence LIMIT 1 OFFSET ?", scope.project, nativeBoardTrimLimit-1).Scan(&floor); err != nil {
				t.Fatal(err)
			}
			f.service.boardRetentionScope = nativeScope{}
			for range len(scopes) + 1 {
				f.service.maintainNativeBoard(t.Context())
				if f.service.boardRetentionScope.project == scope.project {
					break
				}
			}
			var afterCount int64
			if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_deltas WHERE project_id=?", scope.project).Scan(&afterCount); err != nil {
				t.Fatal(err)
			}
			if beforeCount-afterCount != nativeBoardTrimLimit {
				t.Fatalf("trim removed %d rows, want %d", beforeCount-afterCount, nativeBoardTrimLimit)
			}
			var retainedCount int64
			if err := d.reader.QueryRowContext(t.Context(), "SELECT retained_count FROM native_board_sequences WHERE project_id=?", scope.project).Scan(&retainedCount); err != nil || retainedCount != afterCount {
				t.Fatalf("retained count=%d actual=%d: %v", retainedCount, afterCount, err)
			}
			gotHead, gotFloor, err := readDashboardWatermark(t.Context(), d.reader, scope)
			if err != nil || gotHead != beforeHead || gotFloor != floor {
				t.Fatalf("watermarks=(%d,%d) want (%d,%d): %v", gotHead, gotFloor, beforeHead, floor, err)
			}
			for _, since := range []int64{floor - 1, floor} {
				page, err := readDashboardChanges(t.Context(), d.reader, scope, since, 1)
				if err != nil || page.Gap != (since < floor) {
					t.Fatalf("inclusive floor since=%d page=%#v: %v", since, page, err)
				}
				if !page.Gap && (len(page.Changes) != 1 || page.NextSequence != page.Changes[0].Sequence || page.NextSequence >= page.Sequence) {
					t.Fatalf("bounded replay skipped its unread tail: %#v", page)
				}
			}
			if got := head(scope); got != beforeHead {
				t.Fatal("retention generated a tombstone")
			}
		}
	})
	t.Run("empty feed starts at zero", func(t *testing.T) {
		var project string
		if err := d.db.QueryRowContext(t.Context(), "INSERT INTO projects(organization_id,name,profile,created_at) VALUES(?,'Empty dashboard','native',?) RETURNING id", f.project.OrganizationID, formatHubTime(now)).Scan(&project); err != nil {
			t.Fatal(err)
		}
		page, err := readDashboardChanges(context.Background(), d.reader, nativeScope{organization: f.project.OrganizationID, project: tracker.ProjectID(project)}, 0, 10)
		if err != nil || page.Sequence != 0 || page.Floor != 0 || page.Gap || len(page.Changes) != 0 {
			t.Fatalf("empty feed=%#v: %v", page, err)
		}
	})
	t.Run("migration preserves board payloads and backfills typed health scope", func(t *testing.T) {
		tx, err := d.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var payload string
		var sequence int64
		if err := tx.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE project_id=? LIMIT 1", f.project.ID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(t.Context(), "INSERT INTO native_board_deltas(organization_id,project_id,current_json) VALUES(?,?,?) RETURNING sequence", f.project.OrganizationID, f.project.ID, payload).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		raw, err := migrationFiles.ReadFile("migration_steps/20261027013142_dashboard_change_log.sql")
		if err != nil {
			t.Fatal(err)
		}
		_, down, _ := strings.Cut(string(raw), "-- +goose Down")
		if _, err := tx.ExecContext(t.Context(), down); err != nil {
			t.Fatal(err)
		}
		projects, err := json.Marshal([]string{string(f.project.ID), string(other.project.ID)})
		if err != nil {
			t.Fatal(err)
		}
		if err := exec(tx, `INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json)
VALUES('finding_legacy_dashboard',?,'legacy-dashboard','example','flow','{}',?, ?,?,'watch','Legacy finding','Inspect','{}')`, f.project.OrganizationID, string(projects), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		if err := migrateDashboardChangeLog(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		var restored string
		if err := tx.QueryRowContext(t.Context(), "SELECT current_json FROM native_board_deltas WHERE sequence=?", sequence).Scan(&restored); err != nil || restored != payload {
			t.Fatalf("migration altered opaque board payload: %v", err)
		}
		if err := exec(tx, "UPDATE health_findings SET severity='attention' WHERE id='finding_legacy_dashboard'"); err != nil {
			t.Fatal(err)
		}
		for _, scope := range scopes {
			if count(tx, scope, 0, "health", "finding_legacy_dashboard") != 1 {
				t.Fatal("legacy finding did not append in both typed project scopes")
			}
		}
	})
}
