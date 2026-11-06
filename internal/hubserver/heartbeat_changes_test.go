package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHeartbeatChanges(t *testing.T) {
	approved := hubTestPolicy()
	states := nativeFixtureStates()
	states[0].Transitions = append(states[0].Transitions, "Blocked")
	approved.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})}
	f := newRunnerPGFixture(t, func(descriptor *policy.Descriptor) { descriptor.Workflow = approved.Workflow })
	approved = approved.WithID()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT 'org_startup','prj_runner','Blocked',false,false,max(board_rank)+1 FROM project_workflow_states WHERE organization_id='org_startup' AND project_id='prj_runner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO project_workflow_transitions(organization_id,project_id,source_name,target_name,transition_rank) VALUES('org_startup','prj_runner','Blocked','Todo',0),('org_startup','prj_runner','Todo','Blocked',99)`); err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/org_startup/projects/prj_runner"
	binding := f.bindings[0]
	credential := f.tokens[0]
	item := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: "wi_1", Number: 1, Revision: 1}}
	now := time.Now().UTC().Add(time.Second)
	f.service.config.now = func() time.Time { return now }
	f.service.database.now = f.service.config.now
	for _, query := range []string{
		`UPDATE runner_identities SET display_name='Example',expires_at=$1 WHERE organization_id='org_startup'`,
		`UPDATE api_tokens SET expires_at=$1 WHERE organization_id='org_startup'`,
	} {
		if _, err := f.admin.ExecContext(t.Context(), query, now.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE leases SET released_at=$1 WHERE organization_id='org_startup'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Done' WHERE organization_id='org_startup' AND native_id='wi_2'`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: "wi_1"}); err != nil {
			return err
		}
		return w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: "wi_1", Scope: "prj_runner", State: "Todo", Rank: "a", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	activity := func() {
		t.Helper()
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: "org_startup", ProjectID: "prj_runner", AggregateType: "issue", AggregateID: item.WorkItemID, Type: "comment.created", SchemaVersion: 1, RecordedAt: now, Actor: tracker.Actor{Kind: "runner", PrincipalID: "credential_1"}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), "org_startup", raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
	}
	setState := func(state string) {
		t.Helper()
		if err := f.admin.QueryRowContext(t.Context(), `UPDATE issues SET workflow_state_name=$1,revision=revision+1,last_activity_at=$2 WHERE organization_id='org_startup' AND native_id='wi_1' RETURNING revision`, state, now).Scan(&item.Revision); err != nil {
			t.Fatal(err)
		}
	}
	f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	envelope, err := f.service.config.SecretKeys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD("org_startup", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	heartbeat := func() runnerauth.HeartbeatChanges {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, map[string]any{
			"backend_isolation": isolation.Report{"example": {isolation.NativeTrusted}}, "capacity": 2, "version": "test", "display_name": "Example", "os": "linux", "architecture": "amd64", "protocol_major": 1, "change_cursor": cursor,
		})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		if snapshot.Changes == nil || snapshot.Changes.Cursor == "" || snapshot.Changes.CapabilitiesDigest == "" || snapshot.Changes.PolicyID != approved.ID {
			t.Fatalf("missing heartbeat hints: %+v", snapshot)
		}
		cursor = snapshot.Changes.Cursor
		return *snapshot.Changes
	}
	initial := heartbeat()
	if !initial.Reset || !initial.Claimable {
		t.Fatalf("initial=%+v", initial)
	}
	claimable := true
	for _, test := range []struct {
		name    string
		mutate  func()
		reset   bool
		changed bool
	}{
		{name: "unchanged"},
		{name: "item edit", changed: true, mutate: func() {
			if err := f.admin.QueryRowContext(t.Context(), `UPDATE issues SET title='updated',revision=revision+1,last_activity_at=$1 WHERE organization_id='org_startup' AND native_id='wi_1' RETURNING revision`, now).Scan(&item.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "activity without an issue revision", changed: true, mutate: activity},
		{name: "activity outside collaboration history", changed: true, mutate: func() {
			if _, err := f.admin.ExecContext(t.Context(), "UPDATE issues SET last_activity_at=$1 WHERE organization_id='org_startup' AND native_id=$2", now, item.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missed heartbeat", changed: true, mutate: func() {
			previous := cursor
			activity()
			_ = heartbeat()
			cursor = previous
		}},
		{name: "malformed cursor", reset: true, mutate: func() { cursor = "invalid" }},
		{name: "reset cursor", reset: true, mutate: func() { cursor = "" }},
		{name: "expired cursor", reset: true, mutate: func() { now = now.Add(2 * time.Hour) }},
		{name: "caught up"},
		{name: "Blocked queue", changed: true, mutate: func() { setState("Blocked"); claimable = false }},
		{name: "newly claimable queue", changed: true, mutate: func() { setState("Todo"); claimable = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = now.Add(time.Second)
			if test.mutate != nil {
				test.mutate()
			}
			changes := heartbeat()
			if changes.Reset != test.reset || (len(changes.Items) > 0) != test.changed || changes.Claimable != claimable {
				t.Fatalf("changes=%+v", changes)
			}
			if test.changed && (len(changes.Items) != 1 || changes.Items[0].ID != item.WorkItemID || changes.Items[0].Revision != item.Revision) {
				t.Fatalf("delta=%+v", changes.Items)
			}
		})
	}
	before := heartbeat()
	f.service.config.Version = "v9.0.0"
	now = now.Add(time.Second)
	after := heartbeat()
	if before.CapabilitiesDigest == after.CapabilitiesDigest {
		t.Fatal("Hub deploy did not change the capabilities digest")
	}
	next := approved
	next.SourceRevision = strings.Repeat("b", 40)
	next = next.WithID()
	prepared, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), "org_startup", next)
	if err != nil {
		t.Fatal(err)
	}
	prepared.ProjectID = "prj_runner"
	prepared.ApprovedAt = now
	prepared.ApprovedBy = "example"
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error { return w.ApproveProjectPolicy(t.Context(), prepared, approved.ID) }); err != nil {
		t.Fatal(err)
	}
	approved = next
	if changes := heartbeat(); changes.PolicyID != approved.ID {
		t.Fatalf("policy change was not delivered: %+v", changes)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, map[string]any{"capacity": 2, "version": "test", "display_name": "Example", "os": "linux", "architecture": "amd64", "protocol_major": 1, "backend_isolation": isolation.Report{"example": {isolation.NativeTrusted}}})
	requireNativeStatus(t, response, http.StatusOK)
	var legacy runnerauth.RoutingSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &legacy); err != nil || legacy.Changes != nil {
		t.Fatalf("legacy heartbeat=%+v error=%v", legacy, err)
	}
}
