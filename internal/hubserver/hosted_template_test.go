package hubserver

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestHostedProjectTemplateHasReviewLane checks that a project created by a
// hosted owner starts with the review lane a completed run's Change Request
// waits in: neither dispatchable nor terminal, reachable from In Progress by
// the orchestrator, and left by a person to Done or back to In Progress.
func TestHostedProjectTemplateHasReviewLane(t *testing.T) {
	f := newBrowserHostedFixture(t, true)
	project := f.createProject(t, "Template project")
	response := f.page(t, "owner", "/api/v2/organizations/"+f.service.config.Hosted.OrganizationID+"/projects/"+project)
	requireNativeStatus(t, response, http.StatusOK)
	var created tracker.NativeProject
	decodeHubResponse(t, response, &created)
	if !reflect.DeepEqual(created.States, HostedProjectStates()) {
		t.Fatalf("hosted project states = %#v, want the template %#v", created.States, HostedProjectStates())
	}
	byName := map[string]tracker.NativeState{}
	for _, state := range created.States {
		byName[state.Name] = state
	}
	for _, test := range []struct {
		from, to string
	}{
		{"In Progress", "Human Review"},
		{"Human Review", "Done"},
		{"Human Review", "In Progress"},
	} {
		t.Run(test.from+" to "+test.to, func(t *testing.T) {
			from, target := byName[test.from], byName[test.to]
			found := false
			for _, name := range from.Transitions {
				found = found || name == test.to
			}
			if !found || target.OperatorOnly {
				t.Fatalf("%s may not move to %s (operator only %t): %#v", test.from, test.to, target.OperatorOnly, from)
			}
		})
	}
	review := byName["Human Review"]
	if review.Terminal || review.Dispatchable || review.OperatorOnly {
		t.Fatalf("Human Review = %#v, want a non-terminal, non-dispatchable, non-operator lane", review)
	}
}
