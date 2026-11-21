package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestRunnerSessionsUseAcknowledgedRevision(t *testing.T) {
	for _, tc := range []struct {
		name                                                        string
		conflicts                                                   int
		status                                                      int
		code                                                        string
		uncertain, newFence, newLease, switchSession, cancelRefresh bool
		wantReads                                                   int
	}{
		{name: "serial"},
		{name: "foreign revision", conflicts: 1, wantReads: 1},
		{name: "repeated conflict", conflicts: 2, wantReads: 1},
		{name: "cancelled revision refresh", conflicts: 1, cancelRefresh: true, wantReads: 1},
		{name: "unauthorized", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "forbidden", status: http.StatusForbidden, code: "forbidden"},
		{name: "stale fencing", status: http.StatusConflict, code: "stale_fencing_token"},
		{name: "policy mismatch", status: http.StatusConflict, code: "policy_mismatch"},
		{name: "unavailable", status: http.StatusServiceUnavailable, code: "unavailable"},
		{name: "uncertain acknowledgement", uncertain: true},
		{name: "new lease", newLease: true, wantReads: 1},
		{name: "new fence", newFence: true, wantReads: 1},
		{name: "session switch", switchSession: true, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, writes, rejected := 0, 0, 0
			revisions := map[int64]int64{}
			var rejectedKey string
			var cancellation error
			errorEnabled := true
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				response := httptest.NewRecorder()
				response.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					reads++
					if tc.cancelRefresh && rejected > 0 {
						cancellation = runner.ErrOperatorStopped
					}
					var id int64
					if _, err := fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id); err != nil {
						t.Fatal(err)
					}
					if err := json.NewEncoder(response).Encode(runnersession.Snapshot{ID: id, Revision: revisions[id]}); err != nil {
						t.Fatal(err)
					}
					return response.Result(), nil
				}
				var request runnersession.Mutation
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				writes++
				wantLease, wantFence := tracker.LeaseID("lease_example"), tracker.FencingToken(1)
				if request.Start == nil && tc.newLease {
					wantLease = "lease_other"
				}
				if request.Start == nil && tc.newFence {
					wantFence = 2
				}
				if request.LeaseID != wantLease || request.FencingToken != wantFence {
					t.Fatalf("missing execution authority: %+v", request)
				}
				if request.Start != nil {
					id := int64(len(revisions) + 1)
					revisions[id] = 1
					if err := json.NewEncoder(response).Encode(runnersession.Snapshot{ID: id, Revision: 1}); err != nil {
						t.Fatal(err)
					}
					return response.Result(), nil
				}
				if rejected < tc.conflicts {
					rejected++
					revisions[request.SessionID]++
					rejectedKey = request.Key
					response.WriteHeader(http.StatusConflict)
					response.WriteString(`{"code":"revision_conflict"}`)
					return response.Result(), nil
				}
				if tc.status != 0 && errorEnabled {
					response.WriteHeader(tc.status)
					if err := json.NewEncoder(response).Encode(map[string]string{"code": tc.code}); err != nil {
						t.Fatal(err)
					}
					return response.Result(), nil
				}
				if request.ExpectedRevision != revisions[request.SessionID] {
					t.Fatalf("write revision=%d want=%d", request.ExpectedRevision, revisions[request.SessionID])
				}
				if rejectedKey != "" && request.Key == rejectedKey {
					t.Fatal("changed revision reused rejected mutation key")
				}
				revisions[request.SessionID]++
				if tc.uncertain && errorEnabled {
					return nil, errors.New("committed response lost")
				}
				if err := json.NewEncoder(response).Encode(runnersession.Snapshot{ID: request.SessionID, Revision: revisions[request.SessionID]}); err != nil {
					t.Fatal(err)
				}
				return response.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			sessions := NewRunnerSessions(native)
			lease := tracker.NativeLease{ID: "lease_example", FencingToken: 1}
			ctx := WithMutationAuthority(t.Context(), native.base(), lease, func() error { return cancellation })
			id, err := sessions.StartSession(ctx, store.SessionStart{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.switchSession {
				if _, err := sessions.StartSession(ctx, store.SessionStart{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.newLease {
				lease.ID = "lease_other"
				ctx = WithMutationAuthority(t.Context(), native.base(), lease, func() error { return cancellation })
			}
			if tc.newFence {
				lease.FencingToken++
				ctx = WithMutationAuthority(t.Context(), native.base(), lease, func() error { return cancellation })
			}
			err = sessions.UpdateSessionResumeState(ctx, id, store.SessionResumeState{})
			wantError := tc.status != 0 || tc.uncertain || tc.conflicts > 1 || tc.cancelRefresh
			if (err != nil) != wantError {
				t.Fatalf("mutation error=%v wantError=%t", err, wantError)
			}
			if reads != tc.wantReads {
				t.Fatalf("pre-write reads=%d want=%d", reads, tc.wantReads)
			}
			wantWrites := 2
			if tc.switchSession {
				wantWrites++
			}
			if tc.conflicts > 0 && !tc.cancelRefresh {
				wantWrites++
			}
			if writes != wantWrites {
				t.Fatalf("writes=%d want=%d", writes, wantWrites)
			}
			if tc.status == http.StatusUnauthorized || tc.status == http.StatusForbidden || tc.code == "stale_fencing_token" || tc.code == "policy_mismatch" {
				if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("authority refusal lost classification: %v", err)
				}
			}
			if wantError && !tc.cancelRefresh {
				errorEnabled = false
				if err := sessions.UpdateSessionResumeState(ctx, id, store.SessionResumeState{}); err != nil {
					t.Fatal(err)
				}
				if reads != tc.wantReads+1 || writes != wantWrites+1 {
					t.Fatalf("error did not invalidate acknowledged revision: reads=%d writes=%d", reads, writes)
				}
			}

			if tc.name == "serial" {
				if err := sessions.FinishSession(ctx, id, store.SessionFinish{}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{SessionID: id}); err != nil {
					t.Fatal(err)
				}
				if reads != 0 {
					t.Fatalf("completion reads=%d want=0", reads)
				}
			}
		})
	}
}

type sessionLeaseExecution struct {
	runner.Execution
	lease tracker.NativeLease
}

func (e sessionLeaseExecution) Recovery() tracker.NativeRecovery {
	return tracker.NativeRecovery{Lease: e.lease}
}

func (e sessionLeaseExecution) RetryPhasePublication(_ context.Context, send func() error) error {
	return send()
}

func TestDetachedSessionFinalizationSendsUnderLiveFence(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoked=%t", revoked), func(t *testing.T) {
			lease := tracker.NativeLease{ID: "lease_review", FencingToken: 17}
			finalization := &sessionmodel.Finalization{SessionID: 42, Phase: &workflowmetrics.PhaseEvent{}}
			writes := 0
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				response := httptest.NewRecorder()
				if req.Method == http.MethodGet {
					if err := json.NewEncoder(response).Encode(runnersession.Snapshot{ID: 42, Revision: 1}); err != nil {
						t.Fatal(err)
					}
					return response.Result(), nil
				}
				writes++
				var mutation runnersession.Mutation
				if err := json.NewDecoder(req.Body).Decode(&mutation); err != nil || mutation.LeaseID != lease.ID || mutation.FencingToken != lease.FencingToken || mutation.Finalization == nil || !mutation.Finalization.Equal(*finalization) {
					t.Fatalf("finalization = %+v, %v", mutation, err)
				}
				if revoked {
					response.WriteHeader(http.StatusConflict)
					response.WriteString(`{"code":"stale_fencing_token"}`)
				} else {
					if err := json.NewEncoder(response).Encode(runnersession.Snapshot{ID: 42, Revision: 2,
						Finalization: &runnersession.Finalization{Input: *finalization, Finished: true, UsageRecorded: true, PhaseRecorded: true}}); err != nil {
						t.Fatal(err)
					}
				}
				return response.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			sessions := NewRunnerSessions(&NativeClient{client: client, organization: "org_example", project: "prj_example"})
			parent, stop := context.WithCancelCause(t.Context())
			bound := sessions.BindExecution(parent, sessionLeaseExecution{lease: lease})
			stop(context.DeadlineExceeded)
			detached, cancel := context.WithTimeout(context.WithoutCancel(bound), 10*time.Second)
			defer cancel()
			err = sessions.ReconcileSessionFinalization(detached, finalization.SessionID, finalization)
			if writes != 1 || (!revoked && err != nil) || (revoked && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable)) {
				t.Fatalf("finalization sent %d POSTs, result = %v", writes, err)
			}
		})
	}
}
