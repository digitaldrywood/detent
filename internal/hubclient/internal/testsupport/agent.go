package testsupport

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/runner"
)

func ReplyToRoundVerdict(request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (bool, error) {
	if !strings.HasPrefix(request.Prompt, "Judge the completed Detent round") {
		return false, nil
	}
	if !request.ReadOnly || !request.TextOnly || request.Resume != (runner.AgentResume{}) {
		return true, errors.New("round verdict did not use a fresh read-only turn")
	}
	choice := map[string]string{"action": "continue", "next_phase": "implement", "reason": "The fixture assessed the recorded evidence for the current round."}
	switch {
	case strings.Contains(request.Prompt, "\nVerified completion evidence: verified landing"), strings.Contains(request.Prompt, "\nVerified completion evidence: independently verified no-change"):
		choice["action"], choice["next_phase"] = "done", ""
	case strings.Contains(request.Prompt, "\nAvailable phases: merge"):
		choice["next_phase"] = "merge"
	}
	if _, rest, ok := strings.Cut(request.Prompt, "\nExisting human question: "); ok {
		if question, _, _ := strings.Cut(rest, "\n"); question != "" {
			choice["action"], choice["next_phase"], choice["human_action"] = "human_action", "", question
		}
	}
	body, err := json.Marshal(choice)
	if err != nil {
		return true, err
	}
	return true, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: string(body)})
}
