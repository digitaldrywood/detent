package scenariotest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func hubclientEnrolledRunner(t *testing.T, admin *Client, organization tracker.OrganizationID, project tracker.ProjectID, transport http.RoundTripper) (*Client, Machine) {
	t.Helper()
	url := fixtureClient(admin).baseURL.String()
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := hubclientInitializeIdentity(path, url)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Hostname: "example-host", DisplayName: "Example runner", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}, "test": {isolation.Sandbox, isolation.NativeTrusted}}}
	var identity runnerauth.Identity
	redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, Capacity: machine.Capacity, Version: machine.Version, OS: runtime.GOOS, Architecture: runtime.GOARCH, BackendIsolation: machine.BackendIsolation}
	if err := fixtureRunnerRequest(t, admin, enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/runner-enrollments/redeem", redemption, &identity); err != nil {
		t.Fatal(err)
	}
	file.Identity = identity
	if err := hubclientSaveIdentity(path, file); err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleetClient(admin, organization, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.UpdateRunner(t.Context(), identity.RunnerID, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{project}, IsolationTier: isolation.NativeTrusted, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}}}); err != nil {
		t.Fatal(err)
	}
	client, err := createTestClient(t, Config{URL: url, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}}, fixtureClient(admin).constructors)
	if err != nil {
		t.Fatal(err)
	}
	return client, machine
}

func hubclientInitializeIdentity(path, hubURL string) (runnerauth.File, error) {
	credential, err := apikey.GenerateToken()
	if err != nil {
		return runnerauth.File{}, err
	}
	value := runnerauth.File{Schema: 1, HubURL: hubURL, Identity: runnerauth.Identity{Binding: runnerauth.NewBinding()}, Credential: credential}
	return value, hubclientSaveIdentity(path, value)
}

func hubclientSaveIdentity(path string, value runnerauth.File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
