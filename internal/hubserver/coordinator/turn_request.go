package coordinator

import (
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/runner"
)

const (
	TurnTimeout       = 10 * time.Minute
	turnMaxDuration   = 15 * time.Minute
	turnDefaultEffort = "low"
)

func TurnRequest(config conversations.ConversationConfig, hasLuna, modelChoice bool, preferences conversation.Preferences, threadID, prompt string, runErr error) (runner.AgentTurnRequest, runner.AgentBackend, error) {
	request := runner.AgentTurnRequest{
		Workspace:        config.Workspace,
		Prompt:           prompt,
		Resume:           runner.AgentResume{ThreadID: threadID},
		ReadOnly:         true,
		Model:            config.Model,
		ReasoningEffort:  config.ReasoningEffort,
		TurnTimeout:      TurnTimeout,
		MaxDuration:      turnMaxDuration,
		ToolInstructions: Instructions,
	}
	if strings.TrimSpace(request.ReasoningEffort) == "" {
		request.ReasoningEffort = turnDefaultEffort
	}
	backend := config.Backend
	if hasLuna && (!modelChoice || preferences.ModelValue() == "" || preferences.ModelValue() == genkitbackend.Model) {
		if preferences.ModelValue() == genkitbackend.Model {
			request.Model = genkitbackend.Model
		}
		if effort := preferences.EffortValue(); effort == "low" || effort == "medium" {
			request.ReasoningEffort = effort
		}
	} else {
		if model := preferences.ModelValue(); model != "" {
			request.Model = model
		}
		if effort := preferences.EffortValue(); effort != "" {
			request.ReasoningEffort = effort
		}
		if hasLuna && runErr == nil {
			if config.ModelBackend == nil {
				runErr = errors.New("conversation coordinator model backend is unavailable")
			} else {
				backend, request.Workspace, runErr = config.ModelBackend()
				if runErr == nil && backend == nil {
					runErr = errors.New("conversation coordinator model backend is unavailable")
				}
			}
		}
	}
	return request, backend, runErr
}
