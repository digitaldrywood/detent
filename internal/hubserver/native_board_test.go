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
	"strconv"
	"testing"
	"time"

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
			full, err := readNativeWorkSummary(t.Context(), tx, scope, "SELECT i.native_id FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.archived=0", []any{scope.organization, scope.project}, 50, f.service.config.now(), "48h")
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
}

func TestHostedBoardStreamReplay(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "board-viewer", "member", "board@example.test", "write", "")
	item := f.seedIssue(t, 1)
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
	if initial.Gap || len(initial.Deltas) != 0 || initial.Work == nil {
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
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/prj_ungranted/events?board=true&since=0", nil), http.StatusForbidden)
}
