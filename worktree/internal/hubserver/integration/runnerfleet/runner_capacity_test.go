package runnerfleet_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityApplication(t *testing.T) {
	f := hubfixture.NewNative(t, nil, "", "capacity-application")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Enroll(t)
	hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", hubfixture.Policy())
	config := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: time.Now()}
	provider := hubfixture.CapacityReport(time.Now())
	provider.MaxConcurrent = 2
	heartbeat := func() runnerauth.RoutingSnapshot {
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{
			"backend_isolation": r.Redemption.BackendIsolation, "display_name": "runner", "capacity": config.RuntimeLimit, "version": "test", "capacity_configuration": config, "provider_reports": []providercapacity.Report{provider},
		})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		hubfixture.Decode(t, response, &snapshot)
		return snapshot
	}
	heartbeat()
	items := make([]tracker.NativeIssue, 7)
	for i := range items {
		items[i] = f.Create(t, fmt.Sprintf("work-%d", i))
	}
	var original []tracker.NativeLease
	for i := range 2 {
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[i], fmt.Sprintf("session-%d", i)))
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		hubfixture.Decode(t, response, &lease)
		original = append(original, lease)
	}
	var before runnerauth.Runner
	hubfixture.Decode(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil), &before)
	request := runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: config.Revision, Capacity: 6, Backend: "codex"}, ExpectedRevision: before.Revision, IdempotencyKey: "six-workers"}
	response := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/capacity", hubfixture.AdminToken, request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var receipt eventstore.Receipt
	if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		receipt, err = reader.Receipt(t.Context(), hubfixture.AdminCredentialID, "runner_capacity "+r.Binding.RunnerID, request.IdempotencyKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	storedResponse, err := eventstore.NewContent(f.Hub.Config.RunDetails).Receipt(t.Context(), receipt)
	if err != nil || receipt.PayloadObjectKey == "" || !bytes.Equal(bytes.TrimSpace(storedResponse), bytes.TrimSpace(response.Body.Bytes())) {
		t.Fatalf("capacity receipt=%+v error=%v", receipt, err)
	}
	var view runnerauth.CapacityView
	hubfixture.Decode(t, response, &view)
	if view.Desired != 6 || view.Effective == nil || *view.Effective != 2 || view.Status != "partially_applied" {
		t.Fatalf("cloud-only result=%+v", view)
	}
	replay := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/capacity", hubfixture.AdminToken, request)
	hubfixture.RequireStatus(t, replay, http.StatusOK)
	if !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), bytes.TrimSpace(replay.Body.Bytes())) {
		t.Fatalf("replay changed receipt: %s", replay.Body)
	}
	for _, test := range []struct {
		name       string
		change     runnerCapacityChange
		credential string
		path       string
		want       int
	}{
		{"changed retry", runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: config.Revision, Capacity: 7}, ExpectedRevision: before.Revision, IdempotencyKey: request.IdempotencyKey}, hubfixture.AdminToken, r.IdentityPath() + "/capacity", http.StatusConflict},
		{"stale runner", runnerCapacityChange{CapacityRequest: request.CapacityRequest, ExpectedRevision: before.Revision, IdempotencyKey: "stale"}, hubfixture.AdminToken, r.IdentityPath() + "/capacity", http.StatusConflict},
		{"stale configuration", runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("b", 64), Capacity: 6, Backend: "codex"}, ExpectedRevision: before.Revision + 1, IdempotencyKey: "stale-configuration"}, hubfixture.AdminToken, r.IdentityPath() + "/capacity", http.StatusConflict},
		{"worker authority", request, r.Redemption.Credential, r.IdentityPath() + "/capacity", http.StatusForbidden},
		{"other organization", request, hubfixture.AdminToken, "/api/v2/organizations/org_other/runners/" + r.Binding.RunnerID + "/capacity", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, test.path, test.credential, test.change), test.want)
		})
	}
	snapshot := heartbeat()
	if snapshot.Routing.CapacityRequest == nil || snapshot.Routing.CapacityRequest.Capacity != 6 {
		t.Fatalf("directive not delivered=%+v", snapshot)
	}
	config.LocalLimit, config.ClientLimit, config.RuntimeLimit = 6, 6, 6
	config.Revision = strings.Repeat("b", 64)
	heartbeat()
	var stored runnerauth.Runner
	hubfixture.Decode(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil), &stored)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.OrgBase+"/machines/"+string(r.Binding.MachineID)+"/routing", hubfixture.AdminToken, runnerauth.HostChange{ExpectedRevision: stored.HostRevision, DisplayName: stored.HostDisplayName, Capacity: 6}), http.StatusOK)
	read := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/capacity?backend=codex", hubfixture.AdminToken, nil)
	hubfixture.Decode(t, read, &view)
	if view.Effective == nil || *view.Effective != 2 || !strings.Contains(read.Body.String(), "external provider-capacity producer is unmanaged") {
		t.Fatalf("provider ceiling=%s", read.Body)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[2], "third")), http.StatusConflict)
	provider.MaxConcurrent, provider.ObservedAt = 6, time.Now()
	heartbeat()
	for i := 2; i < 6; i++ {
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[i], fmt.Sprintf("session-%d", i))), http.StatusOK)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, hubfixture.ProviderClaim(r, items[6], "seventh")), http.StatusConflict)
	hubfixture.Decode(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil), &stored)
	if stored.Used != 6 || len(stored.ProjectIDs) != len(before.ProjectIDs) || stored.IsolationTier != before.IsolationTier {
		t.Fatalf("runner authority or capacity changed=%+v", stored)
	}
	for _, lease := range original {
		found := false
		for _, current := range stored.Leases {
			if current.ID == lease.ID && current.ProviderReservation != nil && current.ProviderReservation.Report.MaxConcurrent == 2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("active lease/reservation replaced: %s", lease.ID)
		}
	}
	hubfixture.Decode(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/capacity?backend=codex", hubfixture.AdminToken, nil), &view)
	if view.Status != "applied" || view.Effective == nil || *view.Effective != 6 {
		t.Fatalf("applied=%+v", view)
	}
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "capacity-readback-other")
	routing := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: stored.Routing}, IdempotencyKey: "project-ceilings"}
	routing.ProjectIDs = []tracker.ProjectID{f.Project.ID, other.Project.ID}
	routing.ProjectCapacityLimits = map[tracker.ProjectID]int{f.Project.ID: 4, other.Project.ID: 10000}
	response = f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, routing)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &stored)
	read = f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/capacity?backend=codex", hubfixture.AdminToken, nil)
	hubfixture.RequireStatus(t, read, http.StatusOK)
	hubfixture.Decode(t, read, &view)
	project := view.Projects[f.Project.ID]
	inherited := view.Projects[other.Project.ID]
	if view.Status != "applied" || view.Desired != 6 || view.Applied == nil || view.Applied.RuntimeLimit != 6 || view.Effective == nil || *view.Effective != 6 || project.Desired == nil || *project.Desired != 4 || project.Applied == nil || *project.Applied != 4 || project.Effective == nil || *project.Effective != 4 || project.Used != 6 || inherited.Desired == nil || *inherited.Desired != 10000 || inherited.Effective == nil || *inherited.Effective != 6 {
		t.Fatalf("project ceilings distorted effective totals=%+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), r.Redemption.Credential) {
		t.Fatal("credential escaped capacity read")
	}
	if _, err := f.Hub.Admin.ExecContext(t.Context(), "UPDATE runner_identities SET capacity_config_revision=NULL,capacity_config_local_limit=NULL,capacity_config_client_limit=NULL,capacity_config_runtime_limit=NULL,capacity_config_manageable=NULL,capacity_config_constraint=NULL,capacity_config_observed_at=NULL WHERE organization_id=$1 AND runner_id=$2", f.Project.OrganizationID, r.Binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	change := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: stored.Routing}, IdempotencyKey: "lower-total"}
	change.CapacityLimit = 3
	response = f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, change)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	stored = runnerauth.Runner{}
	hubfixture.Decode(t, response, &stored)
	if stored.CapacityRequest != nil || stored.Used != 6 {
		t.Fatalf("superseded request=%+v active leases=%d", stored.CapacityRequest, stored.Used)
	}
}
