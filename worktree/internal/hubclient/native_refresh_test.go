package hubclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeRefreshOversizedAttemptDetails(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"observed states", "epic IDs", "attempt read denied"} {
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
					if mode == "attempt read denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if r.URL.Query().Get("view") != "blockers" {
						_, _ = w.Write(full)
						return
					}
					from, to := 0, 2
					cursor := "next"
					if r.URL.Query().Get("cursor") == "next" {
						from, to, cursor = 2, 4, ""
					}
					projected := append([]tracker.NativeAttempt(nil), attempts[from:to]...)
					for index := range projected {
						projected[index].Runtime = nil
					}
					result = tracker.Page[tracker.NativeAttempt]{Items: projected, NextCursor: cursor}
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
			if mode == "attempt read denied" {
				if err == nil || len(issues) != 0 {
					t.Fatal("read denial became successful observation")
				}
				return
			}
			if err != nil || len(issues) != 1 {
				t.Fatalf("refresh count=%d error=%v", len(issues), err)
			}
			issue := issues[0]
			if reads != 2 || issue.State != "Blocked" || issue.Description != native.Body || issue.WorkpadSignal == nil || issue.WorkpadSignal.HumanAction != "Operator approval required" || issue.Metadata["hub_disposition_attempt_id"] != "attempt_3" || issue.Metadata["hub_disposition_return_state"] != "In Progress" {
				t.Fatalf("refresh lost current authority: reads=%d issue=%+v", reads, issue)
			}
			t.Logf("full attempt payload=%d bytes; refresh preserved operator hold", len(full))
		})
	}
}
