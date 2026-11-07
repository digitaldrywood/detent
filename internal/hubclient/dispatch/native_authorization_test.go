package dispatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeOwnerNegotiatesCapabilities(t *testing.T) {
	h := newNativeChangeHub(t, true)
	for _, test := range []struct {
		name   string
		client *NativeClient
	}{
		{name: "owner", client: h.admin},
		{name: "runner", client: h.native},
	} {
		t.Run(test.name, func(t *testing.T) {
			supported, err := test.client.HubFeature(t.Context(), tracker.NativeRuntimeEvidenceCapability)
			if err != nil || !supported {
				t.Fatalf("native capability negotiation: supported=%t error=%v", supported, err)
			}
		})
	}
	if err := fixtureRunnerRequest(t, h.admin.Transport(), "unknown-token", http.MethodGet, "/api/v2/capabilities", nil, nil); err == nil {
		t.Fatal("capabilities accepted an unknown credential")
	}
}

func TestNativeComposedAuthorizationClaimAndExecution(t *testing.T) {
	h := newNativeChangeHub(t)
	identityPath := h.native.Transport().IdentityFile()
	file, err := runnerauth.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	machine := h.scheduler.machine
	machine.DisplayName = "Selective runner"
	client, err := newTestClient(t, Config{URL: file.HubURL, IdentityFile: identityPath})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleetClient(h.admin.Transport(), h.organization, nil)
	if err != nil {
		t.Fatal(err)
	}
	setRouting := func(state string) {
		t.Helper()
		current, err := fleet.Fleet(t.Context())
		if err != nil || len(current.Runners) != 1 {
			t.Fatalf("fixture runner: %#v, %v", current, err)
		}
		r := current.Runners[0]
		routing := r.Routing
		routing.State = state
		routing.Availability = runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}
		if err := fleet.UpdateRunner(t.Context(), r.RunnerID, runnerauth.RoutingChange{ExpectedRevision: r.Revision, Routing: routing}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
		if err := h.scheduler.Heartbeat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	setRouting("draining")
	var ids [4]string
	for i, test := range []struct {
		title    string
		labels   []string
		priority int
	}{
		{"Unrelated higher priority", []string{"ready"}, 0},
		{"Excluded higher priority", []string{"ready", "canary", "manual"}, 0},
		{"Unlabeled higher priority", nil, 0},
		{"Authorized target", []string{"ready", "canary"}, 2},
	} {
		issue := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: test.title, Body: "Change the README." + issueContractTestSections, Labels: test.labels, State: "Todo", Priority: new(test.priority)})
		ids[i] = string(issue.WorkItemID)
	}
	hint, err := selector.NativeFilterHint(selector.Selector{And: []selector.Selector{{Labels: selector.Labels{Include: []string{"ready"}}}, {Labels: selector.Labels{Include: []string{"canary"}, Exclude: []string{"manual"}}}}}, selector.Context{})
	if err != nil {
		t.Fatal(err)
	}
	observe := func() {
		t.Helper()
		h.scheduler.ObserveNativeAdmission("local", tracker.NativeAdmissionContext{ObservedAt: time.Now().UTC(), PolicyID: h.descriptor.ID, WorkflowStates: []string{"Todo"}, LabelInclude: hint.LabelInclude, LabelExclude: hint.LabelExclude})
		now = now.Add(2 * time.Second)
		if err := h.scheduler.Heartbeat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	observe()
	request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, Filter: hint}
	items, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(items) != 0 {
		t.Fatalf("draining runner claimed work: %#v, %v", items, err)
	}
	for _, id := range ids {
		evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(id), "")
		if err != nil || len(evidence.Admission) != 1 || evidence.Admission[0].ReasonCode != "runner_draining" || evidence.Attempt != nil {
			t.Fatalf("draining admission evidence: %#v, %v", evidence, err)
		}
	}
	setRouting("active")
	observe()
	for i, id := range ids {
		evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(id), "")
		if err != nil || len(evidence.Admission) != 1 || evidence.Admission[0].SelectorObservedAt == nil {
			t.Fatalf("effective selector evidence: %#v, %v", evidence, err)
		}
		want := "skipped"
		if i == 3 {
			want = "ready"
		}
		if evidence.Admission[0].Outcome != want {
			t.Fatalf("selection for %s = %#v, want %s", id, evidence.Admission[0], want)
		}
	}
	items, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(items) != 1 || items[0].ID != ids[3] {
		t.Fatalf("claim = %#v, %v", items, err)
	}
	if _, err = h.scheduler.AdoptClaim(t.Context(), items[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.scheduler.ReleaseClaim(t.Context(), ids[3], "completed"); err != nil {
			t.Error(err)
		}
	}()
	for _, id := range ids[:3] {
		recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(id))
		if err != nil || recovery.Lease.ID != "" || len(recovery.Attempts) != 0 {
			t.Fatalf("excluded item received a native lease or attempt: %#v, %v", recovery, err)
		}
		execution := h.scheduler.RunExecution(id)
		if execution == nil {
			continue
		}
		_, stop, err := execution.Guard(t.Context())
		if err == nil {
			stop()
			t.Fatalf("excluded item %s received execution authority", id)
		}
	}
	execution := h.scheduler.RunExecution(ids[3])
	if execution == nil {
		t.Fatal("authorized target has no execution authority")
	}
	guarded, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
}

type startupCancellationWriter struct{ cancel context.CancelFunc }

func (w startupCancellationWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("conversation.restart_normalized")) {
		w.cancel()
	}
	return len(data), nil
}

func TestNativeConversationStartupFailureStopsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := dispatchPostgresConfig(t, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Content: newHubContentStore(), RunDetails: &archivedRunDetails{}, Conversation: &hubserver.ConversationConfig{Enabled: true}, Logger: slog.New(slog.NewTextHandler(startupCancellationWriter{cancel}, nil))})
	settleWorkers := func() int {
		buffer := make([]byte, 1<<20)
		return bytes.Count(buffer[:runtime.Stack(buffer, true)], []byte("(*conversationService).settleLoop("))
	}
	before := settleWorkers()
	service, err := hubserver.Open(ctx, cfg)
	if service != nil {
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startup cancellation: %v", err)
	}
	if after := settleWorkers(); after != before {
		t.Fatalf("failed startup retained conversation workers: before=%d after=%d", before, after)
	}
}
