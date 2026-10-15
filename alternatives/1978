package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityApplication(t *testing.T) {
	f := newNativeFixture(t, nil, "", "capacity-application")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	config := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: time.Now()}
	provider := capacityReport(time.Now())
	provider.MaxConcurrent = 2
	heartbeat := func() runnerauth.RoutingSnapshot {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{
			"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": config.RuntimeLimit, "version": "test", "capacity_configuration": config, "provider_reports": []providercapacity.Report{provider},
		})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		return snapshot
	}
	heartbeat()
	items := make([]tracker.NativeIssue, 7)
	for i := range items {
		items[i] = f.create(t, fmt.Sprintf("work-%d", i))
	}
	var original []tracker.NativeLease
	for i := range 2 {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, items[i], fmt.Sprintf("session-%d", i)))
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		original = append(original, lease)
	}
	var before runnerauth.Runner
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &before)
	request := runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: config.Revision, Capacity: 6, Backend: "codex"}, ExpectedRevision: before.Revision, IdempotencyKey: "six-workers"}
	response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/capacity", testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	var view runnerauth.CapacityView
	decodeHubResponse(t, response, &view)
	if view.Desired != 6 || view.Effective == nil || *view.Effective != 2 || view.Status != "partially_applied" {
		t.Fatalf("cloud-only result=%+v", view)
	}
	replay := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/capacity", testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusOK)
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
		{"changed retry", runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: config.Revision, Capacity: 7}, ExpectedRevision: before.Revision, IdempotencyKey: request.IdempotencyKey}, testHubAdminToken, r.identityPath() + "/capacity", http.StatusConflict},
		{"stale runner", runnerCapacityChange{CapacityRequest: request.CapacityRequest, ExpectedRevision: before.Revision, IdempotencyKey: "stale"}, testHubAdminToken, r.identityPath() + "/capacity", http.StatusConflict},
		{"stale configuration", runnerCapacityChange{CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("b", 64), Capacity: 6, Backend: "codex"}, ExpectedRevision: before.Revision + 1, IdempotencyKey: "stale-configuration"}, testHubAdminToken, r.identityPath() + "/capacity", http.StatusConflict},
		{"worker authority", request, r.redemption.Credential, r.identityPath() + "/capacity", http.StatusForbidden},
		{"other organization", request, testHubAdminToken, "/api/v2/organizations/org_other/runners/" + r.binding.RunnerID + "/capacity", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, test.path, test.credential, test.change), test.want)
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
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &stored)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.base+"/machines/"+string(r.binding.MachineID)+"/routing", testHubAdminToken, runnerauth.HostChange{ExpectedRevision: stored.HostRevision, DisplayName: stored.HostDisplayName, Capacity: 6}), http.StatusOK)
	read := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity?backend=codex", testHubAdminToken, nil)
	decodeHubResponse(t, read, &view)
	if view.Effective == nil || *view.Effective != 2 || !strings.Contains(read.Body.String(), "external provider-capacity producer is unmanaged") {
		t.Fatalf("provider ceiling=%s", read.Body)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, items[2], "third")), http.StatusConflict)
	provider.MaxConcurrent, provider.ObservedAt = 6, time.Now()
	heartbeat()
	for i := 2; i < 6; i++ {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, items[i], fmt.Sprintf("session-%d", i))), http.StatusOK)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, items[6], "seventh")), http.StatusConflict)
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", r.redemption.Credential, nil), &stored)
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
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity?backend=codex", testHubAdminToken, nil), &view)
	if view.Status != "applied" || view.Effective == nil || *view.Effective != 6 {
		t.Fatalf("applied=%+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), r.redemption.Credential) {
		t.Fatal("credential escaped capacity read")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET capacity_configuration_json='null' WHERE id=?", r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	change := runnerauth.RoutingChange{ExpectedRevision: stored.Revision, Routing: stored.Routing}
	change.CapacityLimit = 3
	response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change)
	requireNativeStatus(t, response, http.StatusOK)
	stored = runnerauth.Runner{}
	decodeHubResponse(t, response, &stored)
	if stored.CapacityRequest != nil || stored.Used != 6 {
		t.Fatalf("superseded request=%+v active leases=%d", stored.CapacityRequest, stored.Used)
	}
}
