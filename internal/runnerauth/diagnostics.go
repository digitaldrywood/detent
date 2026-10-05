package runnerauth

import (
	"errors"
	"slices"
	"time"
)

// LocalChecks contains only observed outcomes, never command output or credentials.
// The Hub stamps ObservedAt when an authenticated runner reports these for a project.
type LocalChecks struct {
	Setup         string    `json:"setup,omitempty"`
	Checkout      string    `json:"checkout"`
	Doctor        string    `json:"doctor"`
	Provider      string    `json:"provider"`
	ProviderKinds []string  `json:"provider_kinds,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

func (c LocalChecks) Validate() error {
	if c.Setup != "" && !slices.Contains([]string{"passed", "failed", "pending"}, c.Setup) {
		return errors.New("local setup check status is invalid")
	}
	for _, state := range []string{c.Checkout, c.Doctor, c.Provider} {
		if !slices.Contains([]string{"passed", "failed", "pending", "warning"}, state) {
			return errors.New("local check status is invalid")
		}
	}
	for i, kind := range c.ProviderKinds {
		if !slices.Contains([]string{"codex", "claude_code", "claude-code", "pi_agent"}, kind) || slices.Contains(c.ProviderKinds[:i], kind) {
			return errors.New("local check provider is invalid")
		}
	}
	return nil
}

func (c LocalChecks) Passed() bool {
	return (c.Setup == "" || c.Setup == "passed") && c.Checkout == "passed" && (c.Doctor == "passed" || c.Doctor == "warning") && c.Provider == "passed"
}
