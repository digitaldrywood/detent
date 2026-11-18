package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProgressReadmissionRecordsCancellationCause(t *testing.T) {
	for _, mode := range []string{runner.RunModeImplement, runner.RunModeMerge} {
		t.Run(mode, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Resume after progress review")
			h.claim(t, issue.ID)
			previous := h.scheduler.nativeClaims[issue.ID].lease
			execution := h.scheduler.RunExecution(issue.ID)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Backend: "example", Model: "example-model", Role: "rework"}); err != nil {
				t.Fatal(err)
			}
			next, _, err := h.scheduler.ReadmitRetryProgress(t.Context(), runner.RunRequest{Issue: issue, Execution: execution, Mode: mode}, nil)
			if err != nil || next == nil {
				t.Fatalf("readmit the implementation step: %v", err)
			}
			recovery, err := h.native.Recovery(t.Context(), previous.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			var finished *tracker.NativeAttempt
			for i := range recovery.Attempts {
				if recovery.Attempts[i].LeaseID == previous.ID {
					finished = &recovery.Attempts[i]
				}
			}
			if finished == nil || finished.Outcome != "cancelled" || finished.ClaimReleasedAt == nil || finished.TerminalFailure == nil {
				t.Fatalf("progress attempt was not settled with a cause: %+v", finished)
			}
			failure := finished.TerminalFailure
			if failure.ErrorClass != "lease_released" || failure.Stage != "lease_release" || !strings.Contains(failure.Error, "progress check completed") || failure.ObservedAt.IsZero() {
				t.Fatalf("progress readmission silently cancelled: %+v", failure)
			}
			if _, err := h.scheduler.AdoptClaim(t.Context(), issue, time.Now()); err != nil {
				t.Fatalf("successor claim is unusable: %v", err)
			}
			if err := next.Start(t.Context(), tracker.NativeExecutionIdentity{Backend: "example", Model: "example-model", Role: mode}); err != nil {
				t.Fatalf("successor cannot start: %v", err)
			}
		})
	}
}

func TestNativeRetryProgressOwnerAndSuccess(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"failed", "succeeded", "empty landing", "different issue", "no change", "no work product", "source recovery", "repeated refusal"} {
		t.Run(outcome, func(t *testing.T) {
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 1}}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "failed-attempt", Outcome: outcome}}}}
			switch outcome {
			case "empty landing":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", Refusal: "the published source has no deliverable to verify"}}
			case "no change":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Finalization = &tracker.NativeFinalization{Changed: false}
			case "no work product":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Identity = &tracker.NativeExecutionIdentity{Role: "code"}
			case "source recovery":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Disposition = &tracker.NativeDisposition{Status: "blocked", ReasonCode: "source_recovery_required"}
			case "repeated refusal":
				recovery.Attempts[0].Outcome = "succeeded"
				recovery.Attempts[0].Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "source_missing", BaseSHA: "base", HeadSHA: "head"}}
				prior := recovery.Attempts[0]
				prior.AttemptID = "prior-attempt"
				recovery.Attempts = append([]tracker.NativeAttempt{prior}, recovery.Attempts...)
			}
			evidence := progresscheck.NativeRequest(recovery, "policy", "machine")
			if outcome == "different issue" {
				recovery.Attempts[0].Outcome = "failed"
				evidence = progresscheck.NativeRequest(recovery, "policy", "machine")
				evidence.IssueID = "wi_another"
			}
			var retained *progresscheck.Record
			reads, writes := 0, 0
			client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/api/v2/capabilities" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":["progress_investigation"]}`)), Request: request}, nil
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/work-items/wi_example") {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"revision":"7"}`)), Request: request}, nil
				}
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
			source := &Scheduler{nativeClaims: map[string]nativeClaim{"wi_example": {source: fixtureConnector(native), lease: tracker.NativeLease{ID: "lease", MachineID: "machine", WorkItemID: "wi_example", FencingToken: 7, PolicyID: "policy"}, recovery: recovery}}}
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
				if err != nil || checked != (outcome != "succeeded") {
					t.Fatalf("checked=%v error=%v", checked, err)
				}
			}
			want := 0
			if outcome != "succeeded" {
				want = 1
			}
			if reads != want || writes != want || backend.calls != want {
				t.Fatalf("reads=%d writes=%d turns=%d", reads, writes, backend.calls)
			}
		})
	}
}

func TestProgressPublicationRefreshesRevisionOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		code       string
		wantWrites int
		wantReads  int
		wantError  bool
	}{
		{"conflict then published", http.StatusConflict, "revision_conflict", 2, 2, false},
		{"repeated conflict stops", http.StatusConflict, "revision_conflict", 2, 2, true},
		{"other error is not retried", http.StatusForbidden, "forbidden", 1, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := progresscheck.NativeRequest(tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: 1}}, Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "failed-attempt", Outcome: "failed"}}}}, "policy", "machine")
			reads, writes := 0, 0
			requestID := ""
			client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/v2/capabilities" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":["progress_investigation"]}`)), Request: req}, nil
				}
				status := http.StatusOK
				var body any
				if req.Method == http.MethodGet {
					reads++
					if strings.HasSuffix(req.URL.Path, "/progress") {
						t.Fatal("publication must refresh item revision only")
					}
					body = tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "prj_example", WorkItemID: "wi_example", Revision: tracker.Revision(6 + reads)}}
				} else {
					writes++
					var input map[string]json.RawMessage
					if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					var id string
					if err := json.Unmarshal(input["request_id"], &id); err != nil {
						t.Fatal(err)
					}
					if requestID == "" {
						requestID = id
					}
					if id != requestID {
						t.Fatal("retry changed request key")
					}
					if string(input["expected_revision"]) != fmt.Sprintf("\"%d\"", 6+reads) {
						t.Errorf("expected current item revision, got %s after %d reads", input["expected_revision"], reads)
					}
					if writes == 1 || test.wantError {
						status = test.status
						body = map[string]string{"code": test.code, "error": "publication refused"}
					} else {
						body = progresscheck.Record{RequestID: id, ProjectID: evidence.ProjectID, IssueID: evidence.IssueID, EvidenceRevision: evidence.EvidenceRevision, Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: "failed-attempt" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair the publication protocol", Evidence: []string{"failed-attempt"}}, CommentID: "comment"}
					}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			owner := &progressOwner{client: native, request: evidence, lease: tracker.NativeLease{ID: "lease", WorkItemID: "wi_example", FencingToken: 7}}
			backend := &nativeProgressBackend{}
			_, checked, err := progresscheck.Check(t.Context(), evidence, backend, owner)
			if (err != nil) != test.wantError || checked == test.wantError || reads != test.wantReads || writes != test.wantWrites || backend.calls != 1 {
				t.Fatalf("checked=%v err=%v reads=%d writes=%d turns=%d", checked, err, reads, writes, backend.calls)
			}
		})
	}
}

func TestProgressPublicationNegotiatesDecisionFields(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprintf("investigation capability=%t", modern), func(t *testing.T) {
			request := progresscheck.Request{ProjectID: "prj_example", IssueID: "wi_example", PolicyID: "policy", EvidenceRevision: "revision", Instance: "machine", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}, AvailableSteps: []string{"implement"}}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			capabilityReads, writes := 0, 0
			client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				var body any
				switch {
				case req.URL.Path == "/api/v2/capabilities":
					capabilityReads++
					features := []string{"native_issues", "scoped_collaboration", "repository_policy"}
					if modern {
						features = append(features, progresscheck.InvestigationCapability)
					}
					body = nativeCapabilities{ProtocolMajors: []int{2}, EventSchemas: []int{1}, Features: features}
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/prj_example"):
					body = tracker.NativeProject{Profile: "native"}
				case req.Method == http.MethodGet:
					body = tracker.NativeIssue{NativeReference: tracker.NativeReference{Revision: 7}}
				default:
					writes++
					var input progresscheck.Publication
					decoder := json.NewDecoder(req.Body)
					if err := decoder.Decode(&input); err != nil {
						t.Fatal(err)
					}
					if input.Decision.Legacy() == modern {
						t.Fatalf("incorrect negotiated shape: %+v", input.Decision)
					}
					if !modern {
						encoded, err := json.Marshal(input.Decision)
						if err != nil {
							t.Fatal(err)
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(encoded, &fields); err != nil {
							t.Fatal(err)
						}
						for _, name := range []string{"diagnosis", "instruction", "convergence"} {
							if _, found := fields[name]; found {
								t.Fatalf("legacy Hub received new field %s", name)
							}
						}
					}
					body = progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: input.Decision, CommentID: "comment"}
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded))), Request: req}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			if err := native.Negotiate(t.Context()); err != nil {
				t.Fatal(err)
			}
			owner := &progressOwner{client: native, request: request, lease: tracker.NativeLease{WorkItemID: "wi_example", ID: "lease", FencingToken: 7}}
			decision := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Reason: "Repair the assertion", Diagnosis: "failed: assertion differs", Instruction: "Repair assertion", Evidence: []string{"failed"}}
			record, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, Decision: decision})
			if err != nil || record.Validate(request) != nil || capabilityReads != 1 || writes != 1 {
				t.Fatalf("publication err=%v capability reads=%d writes=%d record=%+v", err, capabilityReads, writes, record)
			}
			decision.Evidence = []string{"retrieved-comment"}
			decision.RetrievedEvidence = []progresscheck.EvidenceReference{{ID: "retrieved-comment"}}
			if _, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, Decision: decision}); err == nil || writes != 1 {
				t.Fatalf("Hub without retrieved evidence capability received new publication: error=%v writes=%d", err, writes)
			}
		})
	}
}

type nativeProgressBackend struct{ calls int }

func (b *nativeProgressBackend) Decide(context.Context, progresscheck.Request) (progresscheck.Decision, error) {
	b.calls++
	return progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: "failed-attempt" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair the publication protocol", Evidence: []string{"failed-attempt"}}, nil
}
