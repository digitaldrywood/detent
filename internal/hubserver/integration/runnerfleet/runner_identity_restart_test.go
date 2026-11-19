package runnerfleet_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRenewRotateRevokeRestart(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 9, 29, 0, 31, 39, 0, time.UTC))
	now := clock.Now()
	f := hubfixture.NewNative(t, hubfixture.Open(t, hubfixture.Config{Clock: clock.Now}), "", "lifecycle")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Enroll(t)
	if r.Identity.Binding != r.Binding || !r.Identity.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) {
		t.Fatal("enrollment changed binding or expiry")
	}
	if err := f.Hub.Service.Close(); err != nil {
		t.Fatal(err)
	}
	f.Hub = f.Hub.Reopen(t)
	now = now.Add(12 * time.Hour)
	clock.Advance(now.Sub(clock.Now()))
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), http.StatusOK)
	response := f.Hub.Request(t, http.MethodPost, r.IdentityPath()+"/renew", r.Redemption.Credential, struct{}{})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var renewed runnerauth.Identity
	hubfixture.Decode(t, response, &renewed)
	if !renewed.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) || renewed.Binding != r.Binding {
		t.Fatal("renewal changed binding or expiry incorrectly")
	}
	// Production sequence: last renewal 12:31:39Z, stopped 22:17Z,
	// restarted the next day at 12:34Z, after the 12:31:39Z expiry.
	now = time.Date(2026, 9, 29, 22, 17, 0, 0, time.UTC)
	clock.Advance(now.Sub(clock.Now()))
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), http.StatusOK)
	now = time.Date(2026, 9, 30, 12, 34, 0, 0, time.UTC)
	clock.Advance(now.Sub(clock.Now()))
	heartbeatPath := f.Base + "/machines/" + string(r.Binding.MachineID) + "/heartbeat"
	heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, heartbeatPath, r.Redemption.Credential, heartbeat), http.StatusUnauthorized)
	response = f.Hub.Request(t, http.MethodPost, r.IdentityPath()+"/renew", r.Redemption.Credential, struct{}{})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &renewed)
	if renewed.Binding != r.Binding || renewed.OrganizationID != r.Identity.OrganizationID || !renewed.ExpiresAt.Equal(now.Add(runnerauth.CredentialTTL)) {
		t.Fatal("restart renewal changed identity or expiry incorrectly")
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, heartbeatPath, r.Redemption.Credential, heartbeat), http.StatusOK)
	replacement, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, r.IdentityPath()+"/rotate", r.Redemption.Credential, runnerauth.Rotation{Credential: replacement}), http.StatusOK)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), http.StatusUnauthorized)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath(), replacement, nil), http.StatusOK)
	for _, test := range []struct {
		name, method, path string
		body               any
	}{
		{"generic rotation", http.MethodPost, "/api/v1/tokens/" + r.Binding.RunnerID + "/rotate", struct{}{}},
		{"grant widening", http.MethodPost, "/api/v2/tokens/" + r.Binding.RunnerID + "/grants", map[string]any{"organization_id": f.Project.OrganizationID, "project_id": f.Project.ID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.Hub.Request(t, test.method, test.path, hubfixture.AdminToken, test.body)
			if response.Code != http.StatusNotFound && response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("bypass status = %d", response.Code)
			}
		})
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, r.IdentityPath(), hubfixture.AdminToken, nil), http.StatusNoContent)
	if err := f.Hub.Service.Close(); err != nil {
		t.Fatal(err)
	}
	f.Hub = f.Hub.Reopen(t)
	now = now.Add(30 * 24 * time.Hour)
	clock.Advance(now.Sub(clock.Now()))
	for _, path := range []string{r.IdentityPath() + "/renew", r.IdentityPath() + "/rotate", f.Base + "/claims", f.Base + "/machines/" + string(r.Binding.MachineID) + "/heartbeat"} {
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, replacement, struct{}{}), http.StatusUnauthorized)
	}
	var events string
	if err := f.Hub.Admin.QueryRowContext(t.Context(), "SELECT string_agg(kind,',' ORDER BY event_id) FROM runner_identity_events WHERE organization_id=$1 AND runner_id=$2", f.Project.OrganizationID, r.Binding.RunnerID).Scan(&events); err != nil || events != "enrolled,renewed,renewed,rotated,revoked" {
		t.Fatalf("lifecycle audit = %q, err=%v", events, err)
	}
}
