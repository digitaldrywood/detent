package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeProgressBackend struct{ calls int }

func (b *nativeProgressBackend) Decide(context.Context, progresscheck.Request) (progresscheck.Decision, error) {
	b.calls++
	return progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Reason: "Repair the publication protocol", Evidence: []string{"failed-attempt"}}, nil
}

func TestNativeRetryProgressOwnerAndSuccess(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"failed", "succeeded", "different issue"} {
		t.Run(outcome, func(t *testing.T) {
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 1}}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "failed-attempt", Outcome: outcome}}}}
			evidence := progresscheck.NativeRequest(recovery, "policy", "machine")
			if outcome == "different issue" {
				recovery.Attempts[0].Outcome = "failed"
				evidence = progresscheck.NativeRequest(recovery, "policy", "machine")
				evidence.IssueID = "wi_another"
			}
			var retained *progresscheck.Record
			reads, writes := 0, 0
			client, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(request.URL.Path, "/work-items/wi_example/progress") {
					t.Fatalf("unexpected progress route: %s", request.URL.Path)
				}
				var response any
				if request.Method == http.MethodGet {
					reads++
					if request.URL.Query().Get("policy") != "policy" || request.URL.Query().Get("instance") != "machine" {
						t.Fatal("missing progress input binding")
					}
					response = struct {
						Request progresscheck.Request `json:"request"`
						Record  *progresscheck.Record `json:"record"`
					}{evidence, retained}
				} else {
					writes++
					var input progresscheck.Publication
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input.LeaseID != "lease" || input.FencingToken != 7 || input.Instance != "machine" || input.PolicyID != "policy" {
						t.Fatalf("unfenced progress publication: %+v", input.Mutation)
					}
					retained = &progresscheck.Record{RequestID: input.RequestID, ProjectID: evidence.ProjectID, IssueID: evidence.IssueID, EvidenceRevision: evidence.EvidenceRevision, Decision: input.Decision, CommentID: "comment"}
					response = retained
				}
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: request}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			source := &Scheduler{nativeClaims: map[string]nativeClaim{"wi_example": {source: &NativeConnector{client: native}, lease: tracker.NativeLease{ID: "lease", MachineID: "machine", WorkItemID: "wi_example", FencingToken: 7, PolicyID: "policy"}, recovery: recovery}}}
			backend := &nativeProgressBackend{}
			request, owner, err := source.RetryProgress(t.Context(), connector.Issue{ID: "wi_example"})
			if outcome == "different issue" {
				if err == nil || writes != 0 || backend.calls != 0 {
					t.Fatalf("another issue's evidence accepted: err=%v writes=%d calls=%d", err, writes, backend.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, checked, err := progresscheck.Check(t.Context(), request, backend, owner)
				if err != nil || checked != (outcome == "failed") {
					t.Fatalf("checked=%v error=%v", checked, err)
				}
			}
			want := 0
			if outcome == "failed" {
				want = 1
			}
			if reads != want || writes != want || backend.calls != want {
				t.Fatalf("reads=%d writes=%d turns=%d", reads, writes, backend.calls)
			}
		})
	}
}
