package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHeartbeatChanges(t *testing.T) {
	f := newNativeFixture(t, nil, "", "heartbeat-changes")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	approved := hubTestPolicy()
	states := nativeFixtureStates()
	states[0].Transitions = append(states[0].Transitions, "Blocked")
	approved.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})}
	approved = approved.WithID()
	approveHubTestPolicy(t, f.service, f.base+"/policy", approved)
	item := f.create(t, "original")
	now := time.Now().UTC().Add(time.Second)
	f.service.config.now = func() time.Time { return now }
	f.service.database.now = f.service.config.now
	cursor := ""
	heartbeat := func() runnerauth.HeartbeatChanges {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{
			"backend_isolation": r.redemption.BackendIsolation, "capacity": 2, "version": "test", "change_cursor": cursor,
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
			title := "updated"
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit"}, ExpectedRevision: item.Revision, Title: &title})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &item)
		}},
		{name: "activity without an issue revision", changed: true, mutate: func() {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "new evidence"})
			requireNativeStatus(t, response, http.StatusOK)
		}},
		{name: "activity outside collaboration history", changed: true, mutate: func() {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET last_activity_at=? WHERE native_id=?", formatHubTime(now), item.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missed heartbeat", changed: true, mutate: func() {
			previous := cursor
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "missed-comment"}, Body: "missed response"}), http.StatusOK)
			_ = heartbeat()
			cursor = previous
		}},
		{name: "malformed cursor", reset: true, mutate: func() { cursor = "invalid" }},
		{name: "reset cursor", reset: true, mutate: func() { cursor = "" }},
		{name: "expired cursor", reset: true, mutate: func() { now = now.Add(2 * time.Hour) }},
		{name: "caught up"},
		{name: "Blocked queue", changed: true, mutate: func() {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "block"}, ExpectedRevision: item.Revision, State: "Blocked", Reason: "user_requested"})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &item)
			claimable = false
		}},
		{name: "newly claimable queue", changed: true, mutate: func() {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "unblock"}, ExpectedRevision: item.Revision, State: "Todo", Reason: "user_requested"})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &item)
			claimable = true
		}},
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
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: approved.ID, Policy: next}), http.StatusOK)
	approved = next
	if changes := heartbeat(); changes.PolicyID != approved.ID {
		t.Fatalf("policy change was not delivered: %+v", changes)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation})
	requireNativeStatus(t, response, http.StatusOK)
	var legacy runnerauth.RoutingSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &legacy); err != nil || legacy.Changes != nil {
		t.Fatalf("legacy heartbeat=%+v error=%v", legacy, err)
	}
}
