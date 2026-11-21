package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
	retry func(context.Context, func() error) error
}

func (e sessionLeaseExecution) Recovery() tracker.NativeRecovery {
	return tracker.NativeRecovery{Lease: e.lease}
}

func (e sessionLeaseExecution) RetryPhasePublication(ctx context.Context, send func() error) error {
	if e.retry != nil {
		return e.retry(ctx, send)
	}
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

func TestWaitingSessionDoesNotBlockAnotherItemsFinalization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const firstSession, secondSession = 41, 42
		firstLease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease_first", FencingToken: 17}
		secondLease := tracker.NativeLease{WorkItemID: "wi_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ID: "lease_second", FencingToken: 18}
		waiting, hubReady, heartbeat := make(chan struct{}), make(chan struct{}), make(chan struct{})
		releaseHeartbeat := sync.OnceFunc(func() { close(heartbeat) })
		defer releaseHeartbeat()
		finalization := &sessionmodel.Finalization{SessionID: secondSession, Phase: &workflowmetrics.PhaseEvent{}}
		var firstWrite []byte
		var firstPosts, secondPosts int
		client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(req *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			var snapshot runnersession.Snapshot
			if req.Method == http.MethodGet {
				if _, err := fmt.Sscanf(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], "%d", &snapshot.ID); err != nil {
					return nil, err
				}
				snapshot.Revision = 1
			} else {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var mutation runnersession.Mutation
				if err := json.Unmarshal(body, &mutation); err != nil {
					return nil, err
				}
				snapshot.ID, snapshot.Revision = mutation.SessionID, mutation.ExpectedRevision+1
				switch mutation.SessionID {
				case firstSession:
					firstPosts++
					if mutation.LeaseID != firstLease.ID || mutation.FencingToken != firstLease.FencingToken || mutation.Key == "" {
						t.Errorf("first session lost its fenced mutation: %+v", mutation)
					}
					switch firstPosts {
					case 1:
						firstWrite = body
						return nil, io.ErrUnexpectedEOF
					case 2:
						if !bytes.Equal(firstWrite, body) || mutation.ExpectedRevision != 1 {
							t.Errorf("retry changed the original session mutation: %+v", mutation)
						}
					case 3:
						if mutation.ExpectedRevision != 2 || mutation.Resume == nil {
							t.Errorf("next write did not use the acknowledged revision: %+v", mutation)
						}
					}
				case secondSession:
					secondPosts++
					select {
					case <-hubReady:
					default:
						return nil, io.ErrUnexpectedEOF
					}
					if mutation.LeaseID != secondLease.ID || mutation.FencingToken != secondLease.FencingToken || mutation.Finalization == nil || !mutation.Finalization.Equal(*finalization) {
						t.Errorf("second session lost its finalization: %+v", mutation)
					}
					snapshot.Finalization = &runnersession.Finalization{Input: *finalization, Finished: true, UsageRecorded: true, PhaseRecorded: true}
				default:
					t.Errorf("unexpected session mutation: %+v", mutation)
				}
			}
			if err := json.NewEncoder(response).Encode(snapshot); err != nil {
				return nil, err
			}
			return response.Result(), nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		sessions := NewRunnerSessions(&NativeClient{client: client, organization: "org_example", project: "prj_example"})
		first := sessions.BindExecution(t.Context(), sessionLeaseExecution{lease: firstLease, retry: func(ctx context.Context, send func() error) error {
			err := send()
			if !NativeTransportUnavailable(err) {
				return err
			}
			close(waiting)
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-heartbeat:
				return send()
			}
		}})
		second := sessions.BindExecution(t.Context(), sessionLeaseExecution{lease: secondLease})
		firstResult, nextResult, secondResult := make(chan error, 1), make(chan error, 1), make(chan error, 1)
		go func() { firstResult <- sessions.SaveSessionProgress(first, firstSession, store.SessionProgress{}) }()
		select {
		case <-waiting:
		case err := <-firstResult:
			t.Fatalf("first session did not wait: %v", err)
		}
		close(hubReady)
		go func() {
			nextResult <- sessions.UpdateSessionResumeState(first, firstSession, store.SessionResumeState{})
		}()
		go func() {
			detached, cancel := context.WithTimeout(context.WithoutCancel(second), 10*time.Second)
			defer cancel()
			secondResult <- sessions.ReconcileSessionFinalization(detached, secondSession, finalization)
		}()
		synctest.Wait()
		select {
		case err := <-secondResult:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Error("second session could not finalize before the first lease heartbeat")
		}
		if firstPosts != 1 || secondPosts != 1 {
			t.Errorf("before the heartbeat, first session sent %d POSTs and second sent %d", firstPosts, secondPosts)
		}
		releaseHeartbeat()
		if err := <-firstResult; err != nil {
			t.Fatal(err)
		}
		if err := <-nextResult; err != nil {
			t.Fatal(err)
		}
		if firstPosts != 3 {
			t.Fatalf("first session sent %d POSTs, want 3", firstPosts)
		}
	})
}
