package runnerfleet_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerFutureProjectAdmission(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, scenario := range []struct {
		name, scope       string
		approved, deleted bool
		want              int
	}{
		{"organization admits future project", "organization", true, false, http.StatusOK},
		{"project scope stays restricted", "projects", true, false, http.StatusNotFound},
		{"organization requires reported configuration", "organization", false, false, http.StatusUnprocessableEntity},
		{"organization omits deleted project", "organization", true, true, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.CreateOrganization(t, scenario.name), "Initial project")
			r := hubfixture.PrepareRunnerForScope(t, f, scenario.scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.Enroll(t)
			r.ReportCapacity(t)
			later := hubfixture.NewNative(t, h, f.Project.OrganizationID, "Future project")

			descriptor := hubfixture.Policy()
			if scenario.approved {
				hubfixture.ApprovePolicy(t, h, later.Base+"/policy", descriptor)
			}
			item := later.Create(t, "future work")
			if !scenario.approved {
				later.ClearPolicy(t)
				descriptor.SourceRevision = strings.Repeat("b", 40)
				descriptor = descriptor.WithID()
			}
			if scenario.deleted {
				if _, err := h.Admin.ExecContext(t.Context(), "UPDATE projects SET deleted_at=$1 WHERE id=$2", h.Now(), later.Project.ID); err != nil {
					t.Fatal(err)
				}
				var projects []tracker.ProjectID
				var ranks map[tracker.ProjectID]int
				if err := h.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					projects, err = reader.RunnerProjects(t.Context(), r.Binding.RunnerID, runnerstore.MaxRunnerProjectList)
					if err != nil {
						return err
					}
					ranks, err = reader.(runnerfleet.RunnerProjectRankReader).RunnerProjectSchedulingRanks(t.Context(), r.Binding.RunnerID, runnerstore.MaxRunnerProjectList)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, ranked := ranks[later.Project.ID]; ranked || !slices.Equal(projects, []tracker.ProjectID{f.Project.ID}) {
					t.Fatalf("projects=%v ranks=%v advertise deleted %s", projects, ranks, later.Project.ID)
				}
				return
			}
			response := h.Request(t, http.MethodPost, later.Base+"/claims", r.Redemption.Credential, tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: r.Binding.MachineID, SessionID: "future", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			hubfixture.RequireStatus(t, response, scenario.want)
			if scenario.want == http.StatusOK {
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				if lease.WorkItemID != item.WorkItemID {
					t.Fatalf("claimed %s, want %s", lease.WorkItemID, item.WorkItemID)
				}
			}
		})
	}
}
