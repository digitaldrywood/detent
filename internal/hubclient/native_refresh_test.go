package hubclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeStateRefreshOversizedIssuePages(t *testing.T) {
	for _, state := range []string{"Todo", "Human Review"} {
		t.Run(state, func(t *testing.T) {
			items := make([]tracker.NativeIssue, 182)
			for index := range items {
				items[index] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index)), Number: index + 1, Revision: 3}, State: state, Body: strings.Repeat("x", 12<<10)}
			}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if !strings.HasSuffix(r.URL.Path, "/work-items") || r.URL.Query().Get("state") != state {
					t.Errorf("unexpected refresh request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
				if err != nil || limit < 1 {
					t.Error("invalid limit")
					return
				}
				start := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					start, err = strconv.Atoi(cursor)
					if err != nil {
						t.Error(err)
						return
					}
				}
				end := min(start+limit, len(items))
				page := tracker.Page[tracker.NativeIssue]{Items: items[start:end]}
				if end < len(items) {
					page.NextCursor = strconv.Itoa(end)
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			connector, err := NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			got, err := connector.FetchIssuesByStates(t.Context(), []string{state})
			if err != nil || len(got) != len(items) {
				t.Fatalf("refresh count=%d want=%d error=%v", len(got), len(items), err)
			}
			for index, issue := range got {
				if issue.ID != string(items[index].WorkItemID) || issue.Description != items[index].Body || issue.State != state {
					t.Fatalf("refresh lost issue %d", index)
				}
			}
			if reads < 3 || reads > 10 {
				t.Fatalf("unexpected page reads=%d", reads)
			}
		})
	}
}

func TestNativeRefreshOversizedAttemptDetails(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"observed states", "epic IDs", "attempt read denied", "attempt authority oversized"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			native := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_test", Profile: "native", Number: 8, Revision: 3}, State: "Blocked", Body: "retain issue body", CreatedAt: now, UpdatedAt: now}
			var attempts []tracker.NativeAttempt
			for index := range 4 {
				attempts = append(attempts, tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{
					AttemptID: fmt.Sprintf("attempt_%d", index), FencingToken: tracker.FencingToken(index + 1),
					Identity:    &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"},
					Disposition: &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true, FinalSummary: "Operator approval required"},
					Runtime:     &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Command: "focused test", DurationNS: 1, HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), Output: strings.Repeat("<", 64<<10)}},
				}, Status: "succeeded", StartedAt: now, WorkItemRevision: 1})
			}
			full, err := json.Marshal(tracker.Page[tracker.NativeAttempt]{Items: attempts})
			if err != nil || len(full) <= maxResponseBytes {
				t.Fatalf("fixture bytes=%d error=%v", len(full), err)
			}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var result any
				switch {
				case strings.HasSuffix(r.URL.Path, "/attempts"):
					reads++
					if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("direction") != "newest" || r.URL.Query().Get("cursor") != "" {
						t.Error("blocker attempts must read only the newest attempt")
					}
					if r.URL.Query().Get("cursor") != "" {
						result = tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
						break
					}
					if mode == "attempt read denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if mode == "attempt authority oversized" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						if err := json.NewEncoder(w).Encode(map[string]string{"code": "attempt_record_too_large", "message": "Work item wi_test attempt attempt_3 exceeds the 65536-byte page limit"}); err != nil {
							t.Error(err)
						}
						return
					}
					if r.URL.Query().Get("view") != "recorded_blockers" {
						_, _ = w.Write(full)
						return
					}
					projected := append([]tracker.NativeAttempt(nil), attempts[len(attempts)-1:]...)
					for index := range projected {
						projected[index].Runtime = nil
					}
					result = tracker.Page[tracker.NativeAttempt]{Items: projected, NextCursor: "older-attempts"}
				case strings.HasSuffix(r.URL.Path, "/history"):
					if r.URL.Query().Get("view") != "blockers" {
						t.Error("blocker history must retain revision authority without runtime")
					}
					result = tracker.Page[tracker.CollaborationEvent]{Items: []tracker.CollaborationEvent{
						{OrganizationID: native.OrganizationID, ProjectID: native.ProjectID, AggregateID: native.WorkItemID, Type: "run.finished", Actor: tracker.Actor{Kind: "runner"}, Data: tracker.CollaborationData{Revision: 2, Run: &tracker.NativeRunData{AttemptID: "attempt_3", FencingToken: 4}}},
						{OrganizationID: native.OrganizationID, ProjectID: native.ProjectID, AggregateID: native.WorkItemID, AggregateSequence: 2, Type: "workflow.transitioned", RecordedAt: now, Actor: tracker.Actor{Kind: "runner"}, Data: tracker.CollaborationData{Revision: 3, FromState: "In Progress", ToState: "Blocked", Reason: "worker_progress"}},
					}}
				case strings.HasSuffix(r.URL.Path, "/work-items"):
					if r.URL.Query().Get("limit") != "200" || r.URL.Query().Get("state") != "Blocked" {
						t.Error("state page contract changed")
					}
					result = tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{native}}
				case strings.HasSuffix(r.URL.Path, "/work-items/wi_test"):
					result = native
				default:
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(result); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			scoped, err := client.Native(native.OrganizationID, native.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewNativeConnector(scoped)
			if err != nil {
				t.Fatal(err)
			}
			var issues []connector.Issue
			if mode == "observed states" {
				issues, err = c.FetchIssuesByStates(t.Context(), []string{"Blocked"})
			} else {
				issues, err = c.FetchIssueStatesByIDs(t.Context(), []string{"wi_test"})
			}
			if mode == "attempt read denied" || mode == "attempt authority oversized" {
				if err == nil || len(issues) != 0 {
					t.Fatal("read denial became successful observation")
				}
				if !strings.Contains(err.Error(), "prj_test#8") || !strings.Contains(err.Error(), "wi_test") {
					t.Fatalf("refresh warning lost item identity: %v", err)
				}
				if mode == "attempt authority oversized" && !strings.Contains(err.Error(), "attempt_3") {
					t.Fatalf("refresh warning lost attempt identity: %v", err)
				}
				return
			}
			if err != nil || len(issues) != 1 {
				t.Fatalf("refresh count=%d error=%v", len(issues), err)
			}
			issue := issues[0]
			if reads != 1 || issue.State != "Blocked" || issue.Description != native.Body || issue.WorkpadSignal == nil || issue.WorkpadSignal.HumanAction != "Operator approval required" || issue.Metadata["hub_disposition_attempt_id"] != "attempt_3" || issue.Metadata["hub_disposition_return_state"] != "In Progress" {
				t.Fatalf("refresh lost current authority: reads=%d issue=%+v", reads, issue)
			}
			t.Logf("full attempt payload=%d bytes; refresh preserved operator hold", len(full))
		})
	}
}

func TestNativeRefreshOversizedRecordedAttempt(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "In Review"}},
		{Name: "Blocked", Transitions: []string{"In Progress"}},
		{Name: "In Review", Transitions: []string{"Done", "In Progress"}},
		{Name: "Done", Terminal: true},
	}, true)
	issue := h.createInProgress(t, "Oversized attempt")
	h.claim(t, issue.ID)
	owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	if err := owner.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	finished := owner.data
	finished.Sequence++
	finished.Outcome = "succeeded"
	finished.CompletionBody = strings.Repeat("<", 64<<10)
	finished.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true, FinalSummary: "Operator approval required"}
	item := tracker.NativeWorkItemID(issue.ID)
	if err := h.native.AppendEvent(t.Context(), item, tracker.NativeRunEvent{Mutation: nativeMutationKey(), Type: "run.finished", SchemaVersion: 1, Data: finished}); err != nil {
		t.Fatal(err)
	}
	current, err := h.native.Issue(t.Context(), item)
	if err != nil {
		t.Fatal(err)
	}
	mutation := nativeMutationKey()
	mutation.LeaseID, mutation.FencingToken = finished.LeaseID, finished.FencingToken
	if _, err := h.native.Transition(t.Context(), item, tracker.Transition{Mutation: mutation, ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress"}); err != nil {
		t.Fatal(err)
	}
	other := h.createInProgress(t, "Unaffected item")
	if err := h.connector.UpdateIssueState(t.Context(), other.ID, "Blocked"); err != nil {
		t.Fatal(err)
	}
	ready := h.createInProgress(t, "Ready work")
	issues, err := h.connector.FetchIssuesByStates(t.Context(), []string{"Blocked", "In Progress"})
	if err != nil || len(issues) != 3 {
		t.Fatalf("observed-state refresh: items=%d error=%v", len(issues), err)
	}
	for _, got := range issues {
		switch got.ID {
		case issue.ID:
			if got.State != "Blocked" || got.WorkpadSignal == nil || got.WorkpadSignal.HumanAction != "Operator approval required" || got.Metadata["hub_disposition_attempt_id"] != finished.AttemptID || got.Metadata["hub_disposition_return_state"] != "In Progress" {
				t.Fatalf("refresh lost recorded authority: %+v", got)
			}
		case other.ID:
			if got.State != "Blocked" || got.WorkpadSignal != nil || got.Description != other.Description {
				t.Fatalf("neighbor changed during refresh: %+v", got)
			}
		case ready.ID:
			if got.State != "In Progress" || got.WorkpadSignal != nil {
				t.Fatalf("ready item changed during refresh: %+v", got)
			}
		default:
			t.Fatalf("unexpected item %s", got.ID)
		}
	}
	evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
	if err != nil || evidence.CurrentLease == nil || evidence.CurrentLease.ID != finished.LeaseID || evidence.Attempt == nil || evidence.Attempt.AttemptID != finished.AttemptID || evidence.Attempt.FencingToken != finished.FencingToken || !evidence.Attempt.Current {
		t.Fatalf("refresh lost active execution: %+v error=%v", evidence, err)
	}
	if candidates := h.candidates(t); len(candidates) != 0 {
		t.Fatalf("refresh double-dispatched a held lease: %+v", candidates)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	candidates := h.candidates(t)
	if len(candidates) != 1 || candidates[0].ID != ready.ID {
		t.Fatalf("oversized attempt changed claim eligibility: %+v", candidates)
	}
}

func TestNativeRecoveryDoesNotReassembleEvidence(t *testing.T) {
	for _, mode := range []string{"complete", "foreign item", "foreign project", "changed snapshot", "repeated offset", "early end", "corrupt bytes", "denied continuation", "unbounded response"} {
		t.Run(mode, func(t *testing.T) {
			want := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_test"}, Body: strings.Repeat("界<", 90_000)}}
			if mode == "foreign item" {
				want.Issue.WorkItemID = "wi_other"
			}
			if mode == "foreign project" {
				want.Issue.ProjectID = "prj_other"
			}
			content, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			sha := hex.EncodeToString(digest[:])
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if mode == "unbounded response" {
					_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
					return
				}
				start := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					start, err = strconv.Atoi(cursor)
					if err != nil {
						t.Error(err)
						return
					}
				}
				end := min(start+(128<<10), len(content))
				page := &tracker.NativeRecoveryPage{Data: append([]byte(nil), content[start:end]...), SHA256: sha, Offset: int64(start), TotalBytes: int64(len(content))}
				if end < len(content) {
					page.NextCursor = strconv.Itoa(end)
				}
				if start > 0 {
					switch mode {
					case "changed snapshot":
						page.SHA256 = strings.Repeat("0", 64)
					case "repeated offset":
						page.Offset = 0
					case "early end":
						page.NextCursor = ""
					case "corrupt bytes":
						page.Data[0] = 'z'
					case "denied continuation":
						w.WriteHeader(http.StatusForbidden)
						return
					}
				}
				if err := json.NewEncoder(w).Encode(tracker.NativeRecovery{Continuation: page}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			got, err := native.Recovery(t.Context(), "wi_test")
			if err == nil || reads != 1 {
				t.Fatalf("candidate hydration reassembled detail evidence: reads=%d error=%v body=%d", reads, err, len(got.Issue.Body))
			}
			if mode == "unbounded response" && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unbounded response lost size refusal: %v", err)
			}
		})
	}
}

func TestNativeRecoveryRequiredBodyFragments(t *testing.T) {
	for _, mode := range []string{"complete", "changed revision", "foreign item", "foreign project", "changed digest", "repeated offset", "early end", "corrupt bytes", "denied continuation", "oversized record", "missing reference"} {
		t.Run(mode, func(t *testing.T) {
			body := strings.Repeat("<", 153<<10) + strings.Repeat("界", 20<<10)
			content, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			sha := hex.EncodeToString(digest[:])
			ref := tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_test", Revision: 3}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				cursor := r.URL.Query().Get("cursor")
				if cursor == "" {
					if r.Header.Get("X-Detent-Recovery-Body") != "fragments" {
						t.Error("required body capability missing")
					}
					read := &tracker.NativeRecoveryRead{Omitted: []string{"issue.body"}, IssueBody: &tracker.NativeRecoveryBodyRead{Cursor: "0", SHA256: sha, TotalBytes: int64(len(content))}}
					if mode == "oversized record" {
						read.IssueBody.TotalBytes = 6*(256<<10) + 3
					}
					if mode == "missing reference" {
						read.IssueBody = nil
					}
					_ = json.NewEncoder(w).Encode(tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: ref}, Read: read})
					return
				}
				start, err := strconv.Atoi(cursor)
				if err != nil || start < 0 || start >= len(content) {
					t.Errorf("bad cursor %q", cursor)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				end := min(start+(64<<10), len(content))
				page := &tracker.NativeRecoveryPage{NativeReference: ref, Section: "issue_body", RecordID: "wi_test", SHA256: sha, Offset: int64(start), TotalBytes: int64(len(content)), Data: append([]byte(nil), content[start:end]...), NextCursor: strconv.Itoa(end)}
				if start > 0 {
					switch mode {
					case "changed revision":
						page.Revision++
					case "foreign item":
						page.WorkItemID = "wi_other"
					case "foreign project":
						page.ProjectID = "prj_other"
					case "changed digest":
						page.SHA256 = strings.Repeat("0", 64)
					case "repeated offset":
						page.Offset = 0
					case "early end":
						page.NextCursor = ""
					case "corrupt bytes":
						page.Data[0] = 'x'
					case "denied continuation":
						w.WriteHeader(http.StatusForbidden)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(tracker.NativeRecovery{Continuation: page})
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(ref.OrganizationID, ref.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := native.Recovery(t.Context(), ref.WorkItemID)
			if mode == "complete" {
				if err != nil || got.Issue.Body != body || got.Issue.Revision != ref.Revision {
					t.Fatalf("required instructions lost: %v", err)
				}
				if reads != 1+(len(content)+(64<<10)-1)/(64<<10) {
					t.Fatal("hydration read optional history")
				}
			} else if err == nil || got.Issue.WorkItemID != "" {
				t.Fatalf("invalid required body reached dispatch: %s %v", mode, err)
			}
			if (mode == "oversized record" || mode == "missing reference") && reads != 1 {
				t.Fatal("invalid body reference triggered reads")
			}
		})
	}
}

func TestNativeRecoveryMixedHubVersions(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(strconv.FormatBool(large), func(t *testing.T) {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_test"}, Body: "exact source"}
			full := tracker.NativeRecovery{Issue: issue}
			if large {
				for range 18 {
					full.Discussion = append(full.Discussion, tracker.NativeComment{Body: strings.Repeat("x", 60<<10)})
				}
			}
			var modern atomic.Bool
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				upgraded := modern.Load()
				if len(r.URL.Query()) != 1 || r.URL.Query().Get("view") != "recovery" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				result := full
				if upgraded && r.Header.Get("X-Detent-Recovery-Projection") == "bounded" && large {
					result.Discussion = result.Discussion[:2]
					result.Read = &tracker.NativeRecoveryRead{DetailPath: "scoped detail", Omitted: []string{"discussion"}}
				}
				if err := json.NewEncoder(w).Encode(result); err != nil && (!large || upgraded) {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(issue.OrganizationID, issue.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			for _, upgraded := range []bool{false, true, false} {
				modern.Store(upgraded)
				before := reads.Load()
				result, err := native.Recovery(t.Context(), issue.WorkItemID)
				if large && !upgraded {
					if !errors.Is(err, ErrUnavailable) {
						t.Fatalf("old Hub size refusal changed: %v", err)
					}
				} else if err != nil || result.Issue.Body != issue.Body {
					t.Fatalf("mixed-version recovery lost source: %v", err)
				}
				if reads.Load() != before+1 {
					t.Fatal("mixed-version recovery retried unsupported data")
				}
			}
		})
	}
}

func TestNativeStateIDsAvoidProjectRefresh(t *testing.T) {
	for _, archived := range []int{0, 10000} {
		t.Run(strconv.Itoa(archived), func(t *testing.T) {
			items := make(map[string]tracker.NativeIssue, archived+2)
			for index := range archived {
				id := fmt.Sprintf("wi_archived%d", index)
				items[id] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id)}, State: "Done"}
			}
			for _, id := range []string{"wi_one", "wi_two"} {
				items[id] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id)}, State: "In Progress"}
			}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				id := strings.TrimPrefix(r.URL.Path, "/api/v2/organizations/org_test/projects/prj_test/work-items/")
				if id != "wi_one" && id != "wi_two" {
					t.Errorf("unbounded request with %d archived items: %s", archived, r.URL)
					http.Error(w, "project list unavailable", http.StatusGatewayTimeout)
					return
				}
				_ = json.NewEncoder(w).Encode(items[id])
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			native.applyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "reset", Reset: true})
			c, err := NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.FetchIssueStatesByIDs(t.Context(), []string{"wi_one", "wi_two"})
			if err != nil || len(got) != 2 || reads != 2 {
				t.Fatalf("items=%d reads=%d error=%v", len(got), reads, err)
			}
			if got[0].ID != "wi_one" || got[1].ID != "wi_two" {
				t.Fatalf("unexpected IDs: %+v", got)
			}
		})
	}
}
