package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerExecutionHost struct {
	client    *api.NativeClient
	claim     executioncontract.ExecutionClaim
	pinned    policy.Descriptor
	available time.Time
}

func (h *runnerExecutionHost) Now() time.Time                              { return time.Now() }
func (h *runnerExecutionHost) LeaseDuration() time.Duration                { return time.Hour }
func (h *runnerExecutionHost) Transport() *api.Client                      { return h.client.Transport() }
func (h *runnerExecutionHost) MachineID() tracker.MachineID                { return h.claim.Lease.MachineID }
func (h *runnerExecutionHost) ExecutionSource(string) *api.NativeConnector { return h.claim.Source }
func (h *runnerExecutionHost) ExecutionClaim(item string) (executioncontract.ExecutionClaim, bool) {
	return h.claim, item == string(h.claim.Lease.WorkItemID) && !h.claim.Lost
}
func (h *runnerExecutionHost) RegisterExecution(_ context.Context, _ orchestrator.SchedulingRequest, _ connector.Issue, claim executioncontract.ExecutionClaim, execution executioncontract.Execution) error {
	claim.Execution = execution
	h.claim = claim
	return nil
}
func (h *runnerExecutionHost) CheckProjectPolicy(ctx context.Context, _, _ string, p policy.Descriptor) error {
	return h.ValidateExecutionPolicy(ctx, string(h.claim.Lease.WorkItemID), p.ID)
}
func (h *runnerExecutionHost) ValidateExecutionPolicy(ctx context.Context, _ string, id string) error {
	if id != h.claim.Lease.PolicyID {
		return runner.ErrExecutionAuthorityUnavailable
	}
	return h.client.Transport().Request(ctx, http.MethodPost, h.client.Scope()+"/leases/"+string(h.claim.Lease.ID)+"/validate", tracker.NativeLeaseMutation{FencingToken: h.claim.Lease.FencingToken}, nil)
}
func (h *runnerExecutionHost) PinnedExecutionPolicy(string) policy.Descriptor { return h.pinned }
func (h *runnerExecutionHost) ExecutionError(_ context.Context, _ string, _ tracker.FencingToken, err error) error {
	return err
}
func (h *runnerExecutionHost) CleanupExecution(_ context.Context, item string, fence tracker.FencingToken) {
	if item == string(h.claim.Lease.WorkItemID) && fence == h.claim.Lease.FencingToken {
		h.claim.Lost = true
	}
}
func (h *runnerExecutionHost) RenewExecution(ctx context.Context, lease tracker.NativeLease, _ int) error {
	var renewed tracker.NativeLease
	err := h.client.Transport().Request(ctx, http.MethodPost, h.client.Scope()+"/leases/"+string(lease.ID)+"/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 300}, &renewed)
	if err == nil {
		h.claim.Lease = renewed
		h.claim.Deadline = renewed.ExpiresAt
	}
	return err
}
func (h *runnerExecutionHost) RemainingExecution(tracker.NativeLease) time.Duration {
	return time.Until(h.claim.Lease.ExpiresAt)
}
func (h *runnerExecutionHost) ExecutionAvailability(tracker.NativeLease) time.Time {
	return h.available
}
func (h *runnerExecutionHost) SetExecutionAvailability(_ tracker.NativeLease, value time.Time) {
	h.available = value
}
func (h *runnerExecutionHost) ProviderObservations() func() ([]providercapacity.Report, error) {
	return nil
}

type runnerLostAcknowledgment struct {
	next http.RoundTripper
	drop atomic.Bool
}

func (t *runnerLostAcknowledgment) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") && response.StatusCode == http.StatusOK && t.drop.Swap(false) {
		_ = response.Body.Close()
		return nil, errors.New("lost acknowledgment after Hub commit")
	}
	return response, err
}

func TestPostgresNativeExecutionRestartUsesHubReceipts(t *testing.T) {
	f := newRunnerPGFixture(t)
	pinned, err := eventstore.NewContent(f.service.runDetailStore()).ProjectPolicy(t.Context(), f.policy)
	if err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.leases {
		fault := &runnerLostAcknowledgment{next: &http.Transport{}}
		client, err := api.New(api.Config{URL: f.server.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: fault}})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native("org_startup", "prj_runner")
		if err != nil {
			t.Fatal(err)
		}
		path := native.Scope() + "/work-items/" + string(lease.WorkItemID) + "/attempts/" + f.data[index].AttemptID
		restart := func(native *api.NativeClient) executioncontract.Execution {
			var attempt tracker.NativeAttempt
			if err := native.Transport().Request(t.Context(), http.MethodGet, path, nil, &attempt); err != nil {
				t.Fatal(err)
			}
			source, err := api.NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			claim := executioncontract.ExecutionClaim{Source: source, Lease: lease, Deadline: lease.ExpiresAt, Recovery: tracker.NativeRecovery{Lease: lease, Attempts: []tracker.NativeAttempt{attempt}}}
			host := &runnerExecutionHost{client: native, claim: claim, pinned: pinned}
			execution := nativechange.NewExecution(host, claim)
			host.claim.Execution = execution
			if err := execution.Start(t.Context(), *f.data[index].Identity); err != nil {
				t.Fatal(err)
			}
			return execution
		}
		first := restart(native)
		sessionID, err := api.NewRunnerSessions(native).StartSession(api.WithMutationAuthority(t.Context(), native.Scope(), lease, nil), store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: time.Now().UTC(), AgentRole: "code"})
		if err != nil {
			t.Fatal(err)
		}
		observed := tracker.NativeUsage{Provider: "example", Model: "example-model", SourceID: fmt.Sprintf("session_%d", sessionID), UsageKind: "incremental", Revision: 1, From: time.Now().UTC(), Input: 10, Output: 2, Currency: "USD", BillingMode: "metered"}
		fault.drop.Store(true)
		if err := first.(runner.UsageExecution).RecordUsage(t.Context(), observed); err == nil {
			t.Fatal("lost publication acknowledgment was treated as durable success")
		}
		observed.Input, observed.Output, observed.Revision = 15, 3, 2
		fault.drop.Store(true)
		if err := first.(runner.UsageExecution).RecordUsage(t.Context(), observed); err == nil {
			t.Fatal("earlier flush did not exercise lost acknowledgement")
		}
		if err := first.(runner.RuntimeExecution).ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		retained, err := native.IssueTokenSpend(t.Context(), store.IssueIdentity{IssueID: string(lease.WorkItemID)})
		if err != nil || retained.InputTokens != 15 || retained.OutputTokens != 3 {
			t.Fatalf("usage observed during an earlier flush failure was lost: %+v %v", retained, err)
		}
		restartedNative, ctx := f.client(t, index)
		restarted := restart(restartedNative)
		if err := restarted.(runner.UsageExecution).RecordUsage(ctx, observed); err != nil {
			t.Fatal(err)
		}
		spend, err := restartedNative.IssueTokenSpend(ctx, store.IssueIdentity{IssueID: string(lease.WorkItemID)})
		if err != nil || spend.InputTokens != 15 || spend.OutputTokens != 3 {
			t.Fatalf("native execution restart lost or duplicated Hub usage: %+v %v", spend, err)
		}
		if index == 0 {
			if err := restarted.Finish(ctx, "failed"); err != nil {
				t.Fatal(err)
			}
			var finished tracker.NativeAttempt
			if err := restartedNative.Transport().Request(ctx, http.MethodGet, path, nil, &finished); err != nil {
				t.Fatal(err)
			}
			if finished.Status != "failed" || len(finished.Usage) != 1 || finished.Usage[0].Input != 15 {
				t.Fatalf("terminal Hub receipt lost recovered usage: %+v", finished)
			}
		} else {
			if err := restartedNative.Transport().Request(ctx, http.MethodPost, restartedNative.Scope()+"/leases/"+string(lease.ID)+"/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := restarted.(runner.UsageExecution).RecordUsage(ctx, tracker.NativeUsage{Provider: "example", Model: "example-model", Input: 3, Output: 1}); err == nil {
				t.Fatal("revoked execution wrote a usage receipt")
			}
		}
	}
}
