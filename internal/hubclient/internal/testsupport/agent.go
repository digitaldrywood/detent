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
	_, required, _ := strings.Cut(request.Prompt, "The required action is ")
	action, next, ok := strings.Cut(required, " and the required next phase: ")
	if !ok {
		return true, errors.New("round verdict omitted its action or phase")
	}
	phase, _, _ := strings.Cut(next, ".")
	body, err := json.Marshal(map[string]string{"action": action, "next_phase": phase, "reason": "The fixture assessed the recorded evidence for the current round."})
	if err != nil {
		return true, err
	}
	return true, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: string(body)})
}
