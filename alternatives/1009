package conversation

import (
	"errors"
	"fmt"
)

// ErrStaleExecution reports that the expected owner generation is no longer
// current. It is never resolved by redirecting to a replacement attempt.
var ErrStaleExecution = errors.New("stale_execution")

// Owner is the execution ownership tuple carried on every control envelope
// and stored with the conversation's execution state.
type Owner struct {
	AttemptID    string `json:"attempt_id"`
	RunID        string `json:"run_id"`
	LeaseID      string `json:"lease_id"`
	FencingToken int64  `json:"fencing_token"`
	RunnerID     string `json:"runner_id"`
	MachineID    string `json:"machine_id"`
	ThreadID     string `json:"thread_id"`
	TurnID       string `json:"turn_id"`
}

// Expected is the owner generation a client believes is current. Empty
// fields are wildcards.
type Expected struct {
	AttemptID string `json:"attempt_id"`
	TurnID    string `json:"turn_id"`
}

// Matches reports ErrStaleExecution when a non-empty expected field differs
// from the owner tuple.
func (o Owner) Matches(expected Expected) error {
	if expected.AttemptID != "" && expected.AttemptID != o.AttemptID {
		return fmt.Errorf("%w: expected attempt %q, current %q", ErrStaleExecution, expected.AttemptID, o.AttemptID)
	}
	if expected.TurnID != "" && expected.TurnID != o.TurnID {
		return fmt.Errorf("%w: expected turn %q, current %q", ErrStaleExecution, expected.TurnID, o.TurnID)
	}
	return nil
}

// Capabilities lists the controls the bound runner supports.
type Capabilities struct {
	Steer     bool `json:"steer"`
	Interrupt bool `json:"interrupt"`
	Answer    bool `json:"answer"`
	Continue  bool `json:"continue"`
}
