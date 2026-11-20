package hubserver

const (
	coordinatorToolListAttention = "list_attention"
	coordinatorToolExplainIssue  = "explain_issue"
	coordinatorToolProposeIssue  = "propose_issue"
)

// coordinatorToolset executes the coordination tools for one turn.
// Every query is scoped to the conversation's organization; project access
// follows the conversation owner's grants.
type coordinatorToolset struct {
	coordinator *conversationTurnCoordinator
	state       *coordinatorTurnState
}

func newCoordinatorToolset(coordinator *conversationTurnCoordinator, state *coordinatorTurnState) *coordinatorToolset {
	return &coordinatorToolset{coordinator: coordinator, state: state}
}
