package hubserver

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeBoardProjectionAndReplay(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Pushed card")
	other := f.create(t, "Unchanged card")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	read := func() tracker.NativeIssuePage {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=board&state=Todo&limit=2000", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		return page
	}
	initial := read()
	if len(initial.Cards) != 2 || len(initial.Items) != 2 || initial.Sequence == 0 {
		t.Fatalf("initial board = %#v", initial)
	}
	since := initial.Sequence
	expectedTitle := "Edited pushed card"
	for _, test := range []struct {
		name string
		path string
		body any
	}{
		{"edit", "", tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "board-edit"}, ExpectedRevision: issue.Revision, Title: &expectedTitle}},
		{"comment", "/comments", map[string]any{"idempotency_key": "board-comment", "body": "Activity changes only this card"}},
		{"transition", "/workflow", map[string]any{"idempotency_key": "board-transition", "expected_revision": "2", "state": "In Progress", "reason": "user_requested"}},
		{"archive", "/archive", map[string]any{"idempotency_key": "board-archive", "expected_revision": "3"}},
		{"restore", "/restore", map[string]any{"idempotency_key": "board-restore", "expected_revision": "4"}},
		{"complete", "/workflow", map[string]any{"idempotency_key": "board-complete", "expected_revision": "5", "state": "Done", "reason": "user_requested"}},
		{"reopen", "/workflow", map[string]any{"idempotency_key": "board-reopen", "expected_revision": "6", "state": "Todo", "reason": "user_requested"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := http.MethodPost
			if test.name == "edit" {
				method = http.MethodPatch
			}
			response := performHubAPIRequest(t, f.service, method, f.base+"/work-items/"+string(issue.WorkItemID)+test.path, f.token, test.body)
			if response.Code < 200 || response.Code >= 300 {
				t.Fatalf("%s: %d %s", test.name, response.Code, response.Body.String())
			}
			tx, err := f.service.database.reader.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			frame, err := readNativeBoardFrame(t.Context(), tx, scope, since)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Gap || len(frame.Deltas) != 1 || frame.Deltas[0].Current == nil || frame.Deltas[0].Current.Issue.WorkItemID != issue.WorkItemID {
				t.Fatalf("frame = %#v", frame)
			}
			if frame.Deltas[0].Current.Issue.WorkItemID == other.WorkItemID {
				t.Fatal("unchanged card was emitted")
			}
			since = frame.Sequence
			summary, err := readNativeBoardSummary(t.Context(), tx, scope, url.Values{}, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			recount := map[string]tracker.NativeWorkLane{}
			rows, err := tx.QueryContext(t.Context(), "SELECT ws.detent_state,count(*),0 FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.project_id=? AND i.archived=0 GROUP BY ws.detent_state", scope.project)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var lane tracker.NativeWorkLane
				if err := rows.Scan(&lane.State, &lane.Total, &lane.Running); err != nil {
					t.Fatal(err)
				}
				recount[lane.State] = lane
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			actual := map[string]tracker.NativeWorkLane{}
			for _, lane := range summary.Lanes {
				actual[lane.State] = lane
			}
			if !reflect.DeepEqual(actual, recount) {
				t.Fatalf("counts=%v recount=%v", actual, recount)
			}
			full, err := readNativeWorkSummary(t.Context(), tx, scope, "SELECT i.native_id FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.archived=0", []any{scope.organization, scope.project}, 50, f.service.config.now(), "48h", "")
			if err != nil {
				t.Fatal(err)
			}
			if summary.Completed != full.Completed {
				t.Fatalf("completed=%d recount=%d", summary.Completed, full.Completed)
			}
		})
	}
	frame, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, initial.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Gap || len(frame.Deltas) != 7 {
		t.Fatalf("missed deltas = %#v", frame)
	}
	unchanged, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, frame.Sequence)
	if err != nil || unchanged.Gap || len(unchanged.Deltas) != 0 {
		t.Fatalf("unchanged=%#v err=%v", unchanged, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_sequences SET replay_floor=? WHERE project_id=?", initial.Sequence+1, scope.project); err != nil {
		t.Fatal(err)
	}
	gap, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, initial.Sequence)
	if err != nil || !gap.Gap {
		t.Fatalf("gap=%#v err=%v", gap, err)
	}
	history := f.create(t, "Historical workflow card")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(history.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "historical-done"}, ExpectedRevision: history.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(other.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "historical-blocker"}, ExpectedRevision: 1, RelatedWorkItemID: history.WorkItemID, Operation: "add"}), http.StatusOK)
	descriptor := hubTestPolicy()
	for _, test := range []struct {
		name     string
		states   []tracker.NativeState
		terminal bool
		wantRank int
	}{
		{"reordered workflow", []tracker.NativeState{nativeFixtureStates()[2], nativeFixtureStates()[0], nativeFixtureStates()[1]}, true, 1},
		{"historical terminal flag changes", []tracker.NativeState{{Name: "Done", Transitions: []string{"Todo"}}, nativeFixtureStates()[0], nativeFixtureStates()[1], {Name: "Closed", Terminal: true}}, false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := descriptor
			changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: test.states}
			changed = changed.WithID()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
			descriptor = changed
			for _, id := range []tracker.NativeWorkItemID{history.WorkItemID, other.WorkItemID} {
				var raw string
				var rank int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json,lane_rank FROM native_board_items WHERE work_item_id=?", id).Scan(&raw, &rank); err != nil {
					t.Fatal(err)
				}
				var card tracker.NativeBoardCard
				if err := json.Unmarshal([]byte(raw), &card); err != nil {
					t.Fatal(err)
				}
				if id == history.WorkItemID {
					if card.Issue.Terminal != test.terminal || rank != test.wantRank {
						t.Fatalf("historical card terminal=%v rank=%d", card.Issue.Terminal, rank)
					}
				} else if len(card.Issue.Blockers) != 1 || card.Issue.Blockers[0].Terminal != test.terminal {
					t.Fatalf("dependent card blockers=%+v", card.Issue.Blockers)
				}
			}
		})
	}

}

func TestNativeBoardAttemptAndChangeDeltas(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	var since int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_sequences WHERE project_id=?", scope.project).Scan(&since); err != nil {
		t.Fatal(err)
	}
	var version tracker.ChangeVersion
	var lease tracker.NativeLease
	worker := f.worker(t, "board-worker")
	assertDelta := func(t *testing.T, status string, count int) {
		t.Helper()
		frame, err := readNativeBoardFrame(t.Context(), f.service.database.reader, scope, since)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Gap || len(frame.Deltas) == 0 {
			t.Fatalf("frame=%#v", frame)
		}
		for _, delta := range frame.Deltas {
			if delta.Current == nil || delta.Current.Issue.WorkItemID != f.issue.WorkItemID {
				t.Fatalf("wrong delta=%#v", delta)
			}
		}
		card := frame.Deltas[len(frame.Deltas)-1].Current
		if card.Issue.WorkItemID != f.issue.WorkItemID {
			t.Fatalf("wrong card: %s", card.Issue.WorkItemID)
		}
		if card.Change == nil || card.Change.Status != f.detail(t).Summary.Status {
			t.Fatalf("change=%#v", card.Change)
		}
		if status != "" && (card.Attempt == nil || card.Attempt.Status != status || card.Attempt.Count != count) {
			t.Fatalf("attempt=%#v want status %s count %d", card.Attempt, status, count)
		}
		summary, err := readNativeBoardSummary(t.Context(), f.service.database.reader, scope, url.Values{}, f.service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, lane := range summary.Lanes {
			running += lane.Running
		}
		expected := 0
		if status == "running" {
			expected = 1
		}
		if running != expected {
			t.Fatalf("running=%d want %d", running, expected)
		}
		since = frame.Sequence
	}
	for _, test := range []struct {
		name, status string
		count        int
		mutate       func(t *testing.T)
	}{
		{"attempt", "running", 1, func(t *testing.T) {
			lease = claimNativeAttempt(t, f.nativeFixture, worker, "board-machine", "board-session", f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, nativeStartedEvent(lease)), http.StatusOK)
		}},
		{"release", "interrupted", 1, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
		}},
		{"new attempt", "running", 2, func(t *testing.T) {
			lease = claimNativeAttempt(t, f.nativeFixture, worker, "board-machine", "board-session-2", f.issue.WorkItemID)
			start := nativeStartedEvent(lease)
			start.IdempotencyKey = "board-second-start"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		}},
		{"expiry", "interrupted", 2, func(t *testing.T) {
			now = now.Add(91 * time.Second)
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
		}},
		{"version", "", 2, func(t *testing.T) { version = f.publish(t, "board-version", "") }},
		{"review", "", 2, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "board-review"}, Decision: "approved"}), http.StatusOK)
		}},
		{"check", "", 2, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = now.Add(time.Second)
			test.mutate(t)
			assertDelta(t, test.status, test.count)
		})
	}
	unaffected := f.create(t, "Historical card without Change")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(unaffected.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "unaffected-done"}, ExpectedRevision: unaffected.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	historical := f.create(t, "Historical Change card")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(historical.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "change-done"}, ExpectedRevision: historical.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	path := f.base + "/work-items/" + string(historical.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "historical-change"}, Title: "Historical Change"})
	requireNativeStatus(t, response, http.StatusOK)
	var historicalChange tracker.ChangeRequest
	decodeHubResponse(t, response, &historicalChange)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+historicalChange.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "historical-version"}, ChangeVersionInput: changeTestInput()}), http.StatusOK)
	for _, statement := range []string{
		"CREATE TABLE policy_dirty_cards(work_item_id TEXT PRIMARY KEY)",
		"CREATE TRIGGER count_change_policy_cards AFTER INSERT ON native_board_dirty BEGIN INSERT OR IGNORE INTO policy_dirty_cards VALUES(NEW.work_item_id); END",
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	changed := hubTestPolicy()
	changed.Gates.AutoPromote = true
	changed = changed.WithID()
	for _, test := range []struct {
		name   string
		method string
		body   any
	}{
		{"changed policy", http.MethodPut, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: changed}},
		{"revoked policy", http.MethodDelete, map[string]string{"expected_policy_id": changed.ID}},
		{"restored policy", http.MethodPut, policy.Change{Policy: hubTestPolicy()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM policy_dirty_cards"); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, test.method, f.base+"/policy", testHubAdminToken, test.body), map[string]int{http.MethodPut: http.StatusOK, http.MethodDelete: http.StatusNoContent}[test.method])
			ids, err := nativePageIDs(t.Context(), f.service.database.reader, "SELECT work_item_id FROM policy_dirty_cards ORDER BY work_item_id")
			want := []string{string(f.issue.WorkItemID), string(historical.WorkItemID)}
			slices.Sort(want)
			if err != nil || !slices.Equal(ids, want) {
				t.Fatalf("policy dirty cards=%v want=%v err=%v", ids, want, err)
			}
			for _, id := range want {
				var raw string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var card tracker.NativeBoardCard
				if err := json.Unmarshal([]byte(raw), &card); err != nil {
					t.Fatal(err)
				}
				wantStatus := "stale_policy"
				if test.name == "restored policy" {
					wantStatus = "needs_evidence"
					if id == string(f.issue.WorkItemID) {
						wantStatus = "reviewed"
					}
				}
				if card.Change == nil || card.Change.Status != wantStatus {
					t.Fatalf("card %s Change=%+v, want %s", id, card.Change, wantStatus)
				}
			}
		})
	}

}

func TestHostedBoardStreamReplay(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "board-viewer", "member", "board@example.test", "write", "")
	item := f.seedIssue(t, 1)
	private := f
	private.project = "prj_board_blocker"
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) SELECT ?,organization_id,'Board blocker',profile,states_json,created_at,0 FROM projects WHERE id=?", private.project, f.project); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?", private.project, f.project); err != nil {
		t.Fatal(err)
	}
	blocker := private.seedIssue(t, 1)
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies(blocker_issue_id,dependent_issue_id,provenance,created_at,updated_at) SELECT b.id,d.id,'native',?,? FROM issues b,issues d WHERE b.native_id=? AND d.native_id=?", formatHubTime(time.Now()), formatHubTime(time.Now()), blocker, item); err != nil {
		t.Fatal(err)
	}
	if err := f.service.hubTransact(t.Context(), func(*sql.Tx, time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var page tracker.NativeIssuePage
	response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	stream := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(page.Sequence, 10))
	readFrame := func(scanner *bufio.Reader) tracker.NativeBoardFrame {
		t.Helper()
		for {
			kind, data := readHostedEvent(t, scanner)
			if kind != "activity" {
				continue
			}
			var frame tracker.NativeBoardFrame
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				t.Fatal(err)
			}
			return frame
		}
	}
	initial := readFrame(stream)
	if initial.Gap || len(initial.Deltas) != 0 || initial.Work == nil || initial.GrantVersion == "" || initial.GrantVersion != page.GrantVersion {
		t.Fatalf("initial=%#v", initial)
	}
	for index := range 2 {
		response := f.request(t, user, http.MethodPost, f.base+"/work-items/"+string(item)+"/comments", map[string]any{"idempotency_key": fmt.Sprintf("board-stream-%d", index), "body": "Pushed activity"})
		requireNativeStatus(t, response, http.StatusOK)
	}
	replay := readFrame(openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(initial.Sequence, 10)))
	if replay.Gap || len(replay.Deltas) != 2 || replay.Sequence <= initial.Sequence {
		t.Fatalf("replay=%#v", replay)
	}
	for _, delta := range replay.Deltas {
		if delta.Current == nil || delta.Current.Issue.WorkItemID != item {
			t.Fatalf("delta=%#v", delta)
		}
	}
	for readFrame(stream).Sequence != replay.Sequence {
	}
	grantVersion := initial.GrantVersion
	for _, test := range []struct {
		name  string
		grant bool
	}{{"gain", true}, {"lose", false}, {"regain", true}} {
		t.Run(test.name, func(t *testing.T) {
			if test.grant {
				private.grant(t, user, false, false)
			} else if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", user.identity.Subject, private.project); err != nil {
				t.Fatal(err)
			}
			var changed tracker.NativeBoardFrame
			for {
				changed = readFrame(stream)
				if changed.GrantVersion != grantVersion {
					break
				}
			}
			if changed.GrantVersion == "" || changed.Sequence != replay.Sequence || len(changed.Deltas) != 0 {
				t.Fatalf("grant-only frame=%#v", changed)
			}
			grantVersion = changed.GrantVersion
			response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board", nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			if page.GrantVersion != grantVersion || len(page.Cards) != 1 {
				t.Fatalf("grant snapshot=%#v", page)
			}
			card := page.Cards[0]
			if test.grant {
				if len(card.Issue.Blockers) != 1 || card.Issue.Blockers[0].ID != blocker || card.StatusLine != "Blocked · 1" {
					t.Fatalf("granted blocker=%#v", card)
				}
			} else if len(card.Issue.Blockers) != 0 || card.StatusLine != "" {
				t.Fatalf("revoked blocker=%#v", card)
			}
			resumed := readFrame(openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(replay.Sequence, 10)))
			if resumed.GrantVersion != grantVersion || len(resumed.Deltas) != 0 || resumed.Gap {
				t.Fatalf("grant reconnect=%#v", resumed)
			}
		})
	}
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/prj_ungranted/events?board=true&since=0", nil), http.StatusForbidden)
}
