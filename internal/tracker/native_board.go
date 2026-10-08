package tracker

import "time"

type NativeBoardAttempt struct {
	Count     int                      `json:"count"`
	ID        string                   `json:"attempt_id"`
	Status    string                   `json:"status"`
	RunnerID  string                   `json:"runner_id,omitempty"`
	MachineID string                   `json:"machine_id,omitempty"`
	Identity  *NativeExecutionIdentity `json:"identity,omitempty"`
	StartedAt time.Time                `json:"started_at"`
	ExpiresAt time.Time                `json:"expires_at"`
}

type NativeBoardChange struct {
	ID       string                   `json:"change_id"`
	Title    string                   `json:"title"`
	Status   string                   `json:"status"`
	External *ChangeExternalReference `json:"external,omitempty"`
}

type NativeBoardCard struct {
	Issue      NativeIssue         `json:"issue"`
	StatusLine string              `json:"status_line"`
	Attempt    *NativeBoardAttempt `json:"attempt"`
	Change     *NativeBoardChange  `json:"change"`
}

type NativeBoardDelta struct {
	Sequence int64            `json:"sequence"`
	Previous *NativeBoardCard `json:"previous"`
	Current  *NativeBoardCard `json:"current"`
}

type NativeBoardFrame struct {
	Sequence int64              `json:"sequence"`
	Gap      bool               `json:"gap"`
	Deltas   []NativeBoardDelta `json:"deltas"`
	Work     *NativeWorkSummary `json:"work,omitempty"`
}
