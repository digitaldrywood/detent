package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestHostedProjectPolicyAdministration proves a hosted owner can read and
// approve the project policy descriptor on the generic route. Before this the
// route was gated on the instance administrator, which on a hosted hub means
// requireHostedAdministration, whose switch did not know the path: every
// hosted session got an opaque 404, and the Templ-era /onboarding/policy route
// was the only approval a hosted owner had. A member and a viewer must still
// see nothing.
func TestHostedProjectPolicyAdministration(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		role string
		want int
	}{
		{name: "owner approves", role: "owner", want: http.StatusOK},
		{name: "admin approves", role: "admin", want: http.StatusOK},
		{name: "member sees nothing", role: "member", want: http.StatusNotFound},
		{name: "viewer sees nothing", role: "viewer", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newHostedSecurityFixture(t)
			user := f.user(t, test.role, test.role, test.role+"@example.test", "write", "")

			descriptor := hubTestPolicy()
			response := f.request(t, user, http.MethodPut, f.base+"/policy", policy.Change{Policy: descriptor})
			requireNativeStatus(t, response, test.want)
			if test.want != http.StatusOK {
				// The refusal is the same opaque 404 every other hosted
				// administration boundary answers with, and the read is
				// refused the same way.
				if body := response.Body.String(); body != "" && !strings.Contains(body, `"not_found"`) {
					t.Fatalf("refusal body = %s, want the opaque not_found", body)
				}
				requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base+"/policy", nil), test.want)
				return
			}

			// The approval landed, so the descriptor reads back on the same
			// route the hosted session just approved it on.
			read := f.request(t, user, http.MethodGet, f.base+"/policy", nil)
			requireNativeStatus(t, read, http.StatusOK)
			if !strings.Contains(read.Body.String(), descriptor.ID) {
				t.Fatalf("policy read = %s, want the approved descriptor %s", read.Body.String(), descriptor.ID)
			}
		})
	}
}

// TestHostedRunnerEnrollmentGrantsPerNamedProject proves enrolment requires the
// runner grant on the projects the enrolment names, not on every project in
// the organization. The September 11, 2026 dogfood run had to grant the runner
// permission on each project one by one before the enrolment control appeared.
func TestHostedRunnerEnrollmentGrantsPerNamedProject(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		role    string
		granted bool
		names   []tracker.ProjectID
		want    int
	}{
		{name: "member granted the named project enrols", role: "member", granted: true, want: http.StatusCreated},
		{name: "member enrolling for another project is refused", role: "member", granted: true, names: []tracker.ProjectID{"prj_other"}, want: http.StatusNotFound},
		{name: "owner granted anywhere enrols for any project", role: "owner", granted: true, names: []tracker.ProjectID{"prj_other"}, want: http.StatusCreated},
		{name: "member without the runner grant is refused", role: "member", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newHostedSecurityFixture(t)
			seedHostedSecurityProject(t, f, "prj_other", "Other project")
			user := f.user(t, test.role, test.role, test.role+"@example.test", "write", "")
			if test.granted {
				f.grant(t, user, true, true)
			}
			names := test.names
			if len(names) == 0 {
				names = []tracker.ProjectID{f.project}
			}

			request := runnerauth.EnrollmentRequest{
				Binding:    runnerauth.Binding{RunnerID: "runner_" + strings.Repeat("a", 32), MachineID: tracker.MachineID("machine_" + strings.Repeat("b", 32))},
				ProjectIDs: names,
				Operations: []string{"claim"},
				TTLSeconds: 300,
			}
			requireNativeStatus(t, f.request(t, user, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", request), test.want)
		})
	}
}

// seedHostedSecurityProject adds a second native project so an enrolment can
// name a project the member was not granted.
func seedHostedSecurityProject(t *testing.T, f hostedSecurityFixture, id tracker.ProjectID, name string) {
	t.Helper()
	states := `[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]`
	now := formatHubTime(f.service.config.now())
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) VALUES (?,?,?,'native',?,?,0)", id, f.service.config.Hosted.OrganizationID, name, states, now); err != nil {
		t.Fatal(err)
	}
}
