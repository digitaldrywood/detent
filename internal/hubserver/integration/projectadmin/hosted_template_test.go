package projectadmin_test

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedProjectTemplateHasReviewLane(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	owner := h.User(t, "owner", "owner", "owner@example.test", "", "")
	response := h.Request(t, owner, http.MethodPost, "/projects", url.Values{"name": {"Template project"}, "grant_access": {"true"}})
	hubfixture.RequireStatus(t, response, http.StatusSeeOther)
	project := strings.TrimPrefix(response.Header().Get("Location"), "/projects/")
	if !strings.HasPrefix(project, "prj_") {
		t.Fatalf("invalid project location: %q", response.Header().Get("Location"))
	}
	response = h.Request(t, owner, http.MethodGet, hubfixture.ProjectBase(h.Primary, tracker.ProjectID(project)), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var created tracker.NativeProject
	hubfixture.Decode(t, response, &created)
	names := make([]string, 0, len(created.States))
	for _, state := range created.States {
		names = append(names, state.Name)
	}
	if want := []string{"Triage", "Todo", "In Progress", "Blocked", "Human Review", "Merging", "Done", "Backlog", "Cancelled"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("hosted project lanes = %v, want %v", names, want)
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
		{"Triage", "Todo"},
		{"Triage", "Backlog"},
		{"Triage", "Cancelled"},
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
	triage := byName["Triage"]
	if triage.Terminal || triage.Dispatchable || triage.OperatorOnly {
		t.Fatalf("Triage must be a non-dispatchable intake lane: %#v", triage)
	}
	review := byName["Human Review"]
	if review.Terminal || review.Dispatchable || review.OperatorOnly {
		t.Fatalf("Human Review = %#v, want a non-terminal, non-dispatchable, non-operator lane", review)
	}
}
