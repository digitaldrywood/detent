package hubclient

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeComposedAuthorizationClaimAndExecution(t *testing.T) {
	h := newNativeChangeHub(t)
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := h.scheduler.machine
	machine.ID = file.Identity.MachineID
	machine.DisplayName = "Selective runner"
	machine.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
	if _, err := EnrollRunner(t.Context(), identityPath, h.organization, enrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleetClient(h.admin.client, h.organization, nil)
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
		priority string
	}{
		{"Unrelated higher priority", []string{"ready"}, "0"},
		{"Excluded higher priority", []string{"ready", "canary", "manual"}, "0"},
		{"Unlabeled higher priority", nil, "0"},
		{"Authorized target", []string{"ready", "canary"}, "2"},
	} {
		issue := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: test.title, Body: "Change the README." + issueContractTestSections, Labels: test.labels, State: "Todo"})
		if err = h.connector.SetField(t.Context(), string(issue.WorkItemID), "priority", test.priority); err != nil {
			t.Fatal(err)
		}
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
