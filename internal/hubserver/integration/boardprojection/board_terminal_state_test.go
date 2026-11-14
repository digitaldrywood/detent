package boardprojection_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardCardsFollowCurrentTerminalStates(t *testing.T) {
	t.Parallel()
	h := quietHub(t)
	p := hubfixture.NewNative(t, h, "", "Example terminal states")
	finished := p.Create(t, "Finished card")
	hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, p.Base+"/work-items/"+string(finished.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "finished"}, ExpectedRevision: finished.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
	dependent := p.Create(t, "Dependent card")
	hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, p.Base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "blocker"}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: finished.WorkItemID, Operation: "add"}), http.StatusOK)
	read := func(t *testing.T) (viewstore.BoardRecord, viewstore.BoardRecord, viewstore.BoardSequence) {
		t.Helper()
		var card, blocked viewstore.BoardRecord
		var sequence viewstore.BoardSequence
		if err := h.Repository.ReadTenant(t.Context(), p.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			if card, err = r.BoardItem(t.Context(), p.Project.ID, finished.WorkItemID); err != nil {
				return err
			}
			if blocked, err = r.BoardItem(t.Context(), p.Project.ID, dependent.WorkItemID); err != nil {
				return err
			}
			sequence, err = r.BoardSequence(t.Context(), p.Project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return card, blocked, sequence
	}
	descriptor := hubfixture.Policy()
	for _, test := range []struct {
		name     string
		states   []tracker.NativeState
		terminal bool
	}{
		{"terminal state becomes active", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}}, {Name: "Done", Transitions: []string{"Todo", "Closed"}}, {Name: "Closed", Terminal: true}}, false},
		{"active state becomes terminal", hubfixture.States(), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h.RefreshBoard(t, p.Organization())
			if card, _, _ := read(t); card.Terminal == test.terminal {
				t.Fatalf("fixture card already terminal=%v", card.Terminal)
			}
			changed := descriptor
			changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: test.states}
			changed = changed.WithID()
			hubfixture.RequireStatus(t, h.Request(t, http.MethodPut, p.Base+"/policy", h.AdminToken(), policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
			descriptor = changed
			_, _, before := read(t)
			h.RefreshBoard(t, p.Organization())
			card, blocked, after := read(t)
			if card.Terminal != test.terminal || card.PageIssue == nil || card.PageIssue.Terminal != test.terminal || card.ListIssue == nil || card.ListIssue.Terminal != test.terminal {
				t.Fatalf("card terminal=%v page=%+v list=%+v, want %v", card.Terminal, card.PageIssue, card.ListIssue, test.terminal)
			}
			if blocked.PageIssue == nil || len(blocked.PageIssue.Blockers) != 1 || blocked.PageIssue.Blockers[0].Terminal != test.terminal {
				t.Fatalf("dependent card blockers=%+v, want terminal %v", blocked.PageIssue, test.terminal)
			}
			if after.Sequence <= before.Sequence {
				t.Fatalf("board sequence %d did not advance from %d", after.Sequence, before.Sequence)
			}
			h.RefreshBoard(t, p.Organization())
			if replayCard, _, replay := read(t); replay.Sequence != after.Sequence || replayCard.Terminal != test.terminal {
				t.Fatalf("settled refresh changed sequence %d to %d or terminal to %v", after.Sequence, replay.Sequence, replayCard.Terminal)
			}
		})
	}
}
