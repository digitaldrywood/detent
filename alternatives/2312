package runnerfleet_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerConcurrentRotationHasOneWinner(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read)
	r.Enroll(t)
	start := make(chan struct{})
	statuses := make(chan int, 4)
	var workers sync.WaitGroup
	for range 4 {
		replacement, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			<-start
			response := f.Hub.Request(t, http.MethodPost, r.IdentityPath()+"/rotate", r.Redemption.Credential, runnerauth.Rotation{Credential: replacement})
			statuses <- response.Code
		})
	}
	close(start)
	workers.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusUnauthorized {
			t.Fatalf("rotation status = %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("rotation winners = %d, want 1", winners)
	}
}

func TestRunnerIdentityBindingAndOperations(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Enroll(t)
	r.ReportCapacity(t)
	other := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	other.Enroll(t)
	other.ReportCapacity(t)
	for _, test := range []struct {
		name    string
		status  int
		unknown string
		admin   bool
	}{
		{name: "", status: http.StatusOK},
		{name: "another-host", status: http.StatusUnprocessableEntity},
		{name: r.Redemption.Hostname, status: http.StatusOK},
		{name: r.Redemption.Hostname, unknown: "top-level", status: http.StatusOK},
		{name: r.Redemption.Hostname, unknown: "nested", status: http.StatusOK},
		{unknown: "top-level", admin: true, status: http.StatusUnprocessableEntity},
		{unknown: "nested", admin: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(fmt.Sprintf("Sprite heartbeat %s unknown=%s admin=%t", test.name, test.unknown, test.admin), func(t *testing.T) {
			body := map[string]any{"display_name": r.Redemption.DisplayName, "capacity": r.Redemption.Capacity, "version": "develop-040e7195c", "sprite_name": test.name, "backend_isolation": r.Redemption.BackendIsolation}
			switch test.unknown {
			case "top-level":
				body["future_observation"] = map[string]any{"enabled": true}
			case "nested":
				body["project_configuration"] = map[string]any{
					"project_id": string(f.Project.ID), "authority": "local_global_configuration",
					"source": "configured_committed_workflow", "observed_at": f.Hub.Now(),
					"future_observation": true,
				}
			}
			token := r.Redemption.Credential
			if test.admin {
				token = hubfixture.AdminToken
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", token, body), test.status)
			if test.status == http.StatusOK {
				if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					machine, err := reader.Machine(t.Context(), string(r.Binding.MachineID))
					if err == nil && (machine.SpriteName != test.name || machine.Version != "develop-040e7195c") {
						t.Fatalf("heartbeat Sprite identity = %q, version = %q", machine.SpriteName, machine.Version)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	issue := f.Create(t, "work")
	descriptor := hubfixture.Policy()
	descriptor.Requirements.RunnerID = r.Binding.RunnerID
	descriptor.Requirements.MachineID = string(r.Binding.MachineID)
	descriptor = descriptor.WithID()
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	otherClaim := claim
	otherClaim.MachineID = other.Binding.MachineID
	otherClaim.SessionID = "other-session"
	response = f.Hub.Request(t, http.MethodPost, f.Base+"/claims", other.Redemption.Credential, otherClaim)
	hubfixture.RequireStatus(t, response, http.StatusConflict)
	if !strings.Contains(response.Body.String(), "selector_no_match") {
		t.Fatal("an enrolled runner widened the approved selector")
	}
	for _, test := range []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"impersonated claim", http.MethodPost, f.Base + "/claims", claim, http.StatusNotFound},
		{"impersonated registration", http.MethodPost, f.Base + "/machines/register", map[string]any{"id": r.Binding.MachineID, "hostname": "customer-host", "display_name": "Same host", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"new machine registration", http.MethodPost, f.Base + "/machines/register", map[string]any{"id": runnerauth.NewBinding().MachineID, "hostname": "customer-host", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"impersonated heartbeat", http.MethodPost, f.Base + "/machines/" + string(r.Binding.MachineID) + "/heartbeat", map[string]any{"display_name": "same", "capacity": 1, "version": "test"}, http.StatusNotFound},
		{"impersonated lease", http.MethodPost, f.Base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}, http.StatusNotFound},
		{"impersonated release", http.MethodPost, f.Base + "/leases/" + string(lease.ID) + "/release", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}, http.StatusNotFound},
		{"impersonated identity", http.MethodGet, r.IdentityPath(), nil, http.StatusNotFound},
		{"v1 downgrade", http.MethodGet, "/api/v1/work-items", nil, http.StatusNotFound},
		{"collaboration without grant", http.MethodPost, f.Base + "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "denied"}, Title: "denied", State: "Todo"}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			hubfixture.RequireStatus(t, f.Hub.Request(t, test.method, test.path, other.Redemption.Credential, test.body), test.want)
		})
	}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "event"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}, LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt"), PolicyID: descriptor.ID}}
	path := f.Base + "/work-items/" + string(issue.WorkItemID)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", other.Redemption.Credential, event), http.StatusNotFound)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", r.Redemption.Credential, event), http.StatusOK)
	response = f.Hub.Request(t, http.MethodGet, path+"/history", r.Redemption.Credential, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), r.Binding.RunnerID) {
		t.Fatal("history did not attribute event to authenticated runner")
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.OrgBase+"/machines/"+string(r.Binding.MachineID)+"/routing", hubfixture.AdminToken, runnerauth.HostChange{ExpectedRevision: 1, DisplayName: "Renamed", Capacity: 2}), http.StatusOK)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "Worker override", "capacity": 2, "version": "test"}), http.StatusOK)
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		machine, err := reader.Machine(t.Context(), string(r.Binding.MachineID))
		if err == nil && (machine.ID != string(r.Binding.MachineID) || machine.DisplayName != "Renamed") {
			t.Fatal("rename did not preserve identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	otherProject := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "other-project")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, otherProject.Base, r.Redemption.Credential, nil), http.StatusNotFound)
	readOnly := hubfixture.PrepareRunner(t, f, runnerauth.Read)
	readOnly.Enroll(t)
	for _, denied := range []string{f.Base + "/claims", f.Base + "/machines/register", path + "/events"} {
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, denied, readOnly.Redemption.Credential, struct{}{}), http.StatusForbidden)
	}
	for _, key := range []string{"provider_api_key", "storage_credentials", "prompt"} {
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		values := make(map[string]any)
		if err := json.Unmarshal(body, &values); err != nil {
			t.Fatal(err)
		}
		values[key] = "example-secret-do-not-store"
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", r.Redemption.Credential, values), http.StatusUnprocessableEntity)
	}
}
