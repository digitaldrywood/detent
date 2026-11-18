package runner

import "github.com/digitaldrywood/detent/internal/runner/prompt"

// Retain only the latest thread baseline for each issue/workspace/backend.
// This is deliberately process-local: an unknown resumed thread gets full context.
type sessionPrompt struct {
	resume AgentResume
	text   string
}

func (r *Runner) followupPrompt(key string, resume AgentResume, prompt string) string {
	r.mu.RLock()
	previous, ok := r.promptHistory[key]
	r.mu.RUnlock()
	if !ok || agentResumeEmpty(resume) || !samePromptThread(previous.resume, resume) {
		return prompt
	}
	return promptDelta(previous.text, prompt)
}

func samePromptThread(a, b AgentResume) bool {
	if a.ThreadID != "" || b.ThreadID != "" {
		return a.ThreadID != "" && a.ThreadID == b.ThreadID
	}
	return a.SessionID != "" && a.SessionID == b.SessionID
}

func (r *Runner) rememberPrompt(key, prompt string, execution agentTurnExecution) {
	resume := AgentResume{ThreadID: execution.turnResult.ThreadID, SessionID: execution.turnResult.SessionID}
	if agentResumeEmpty(resume) || (!execution.turnStarted && execution.err != nil) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promptHistory == nil {
		r.promptHistory = make(map[string]sessionPrompt)
	}
	r.promptHistory[key] = sessionPrompt{resume: resume, text: prompt}
}

func promptDelta(previous, current string) string {
	return prompt.Delta(previous, current)
}
