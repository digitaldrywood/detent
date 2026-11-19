package boardprojection_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeBoardPolicyRefreshIsolatesProjects(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	type projectCards struct {
		fixture hubfixture.Native
		policy  policy.Descriptor
		items   []tracker.NativeIssue
		changes []tracker.ChangeRequest
		want    string
	}
	projects := []projectCards{}
	for _, test := range []struct {
		name string
		want string
	}{
		{"changed-audit", "stale_policy"},
		{"unchanged-gates", "reviewed"},
	} {
		f := hubfixture.NewNative(t, h, h.Primary, test.name)
		descriptor := hubfixture.Policy()
		cards := projectCards{fixture: f, policy: descriptor, want: test.want}
		for _, state := range []string{"Todo", "Done"} {
			response := f.Request(t, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{
				Mutation: tracker.Mutation{IdempotencyKey: state}, Title: test.name + " " + state,
				Body: testenv.DistinctText(test.name+state, 24) + "\n" + hubfixture.IssueContractSections, State: state,
			})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			hubfixture.Decode(t, response, &issue)
			path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = f.Request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: test.name + " " + state})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			hubfixture.Decode(t, response, &change)
			input := hubfixture.ChangeInput()
			input.PolicyID = descriptor.ID
			response = f.Request(t, http.MethodPost, path+"/"+change.ID+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			cards.items = append(cards.items, issue)
			cards.changes = append(cards.changes, change)
		}
		cards.policy.SourceDigest = policy.Digest([]byte(test.name))
		cards.policy.Gates.SecurityAudit = test.name == "changed-audit"
		cards.policy = cards.policy.WithID()
		projects = append(projects, cards)
	}
	for _, project := range projects {
		hubfixture.ApprovePolicy(t, h, project.fixture.Base+"/policy", project.policy)
	}
	h.RefreshBoard(t, h.Primary)
	for _, project := range projects {
		f := project.fixture
		response := f.Request(t, http.MethodGet, f.Base+"/work-items?include=board", nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		hubfixture.Decode(t, response, &page)
		for index, issue := range project.items {
			cardIndex := slices.IndexFunc(page.Cards, func(card tracker.NativeBoardCard) bool {
				return card.Issue.WorkItemID == issue.WorkItemID
			})
			if cardIndex < 0 {
				t.Fatalf("project %s board omitted %s", f.Project.ID, issue.WorkItemID)
			}
			card := page.Cards[cardIndex]
			if card.Issue.State != issue.State || card.Change == nil || card.Change.Status != project.want {
				t.Fatalf("project %s card lane = %s, Change = %+v, want unchanged lane and %s", f.Project.ID, card.Issue.State, card.Change, project.want)
			}
			response := f.Request(t, http.MethodGet, f.Base+"/work-items/"+string(issue.WorkItemID)+"/changes/"+project.changes[index].ID, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var detail tracker.ChangeDetail
			hubfixture.Decode(t, response, &detail)
			if detail.Summary.Status != card.Change.Status {
				t.Fatalf("card status differs from durable detail: %+v", detail.Summary)
			}
		}
	}
}
