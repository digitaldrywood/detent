package tracker

import "time"

const NativeExecutionCapability = "fenced_run_history"

type NativeExecutionIdentity struct {
	Role    string `json:"role"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

type NativeCheckpoint struct {
	Resume          string                 `json:"resume"`
	Availability    string                 `json:"availability"`
	Storage         string                 `json:"storage"`
	WorktreeState   string                 `json:"worktree_state"`
	HeadSHA         string                 `json:"head_sha,omitempty"`
	WorkspaceDigest string                 `json:"workspace_digest,omitempty"`
	ExpectedHeadSHA string                 `json:"expected_head_sha,omitempty"`
	ExternalEffect  string                 `json:"external_effect"`
	EffectState     string                 `json:"effect_state"`
	EffectID        string                 `json:"effect_id,omitempty"`
	Change          *NativeChangeReference `json:"change,omitempty"`
}

type NativeChangeReference struct {
	ChangeID  string `json:"change_id"`
	VersionID string `json:"version_id"`
	HeadSHA   string `json:"head_sha"`
}

type NativeAttempt struct {
	NativeRunData
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Checkpoint *NativeCheckpoint `json:"checkpoint,omitempty"`
}

type NativeRecovery struct {
	Lease      NativeLease            `json:"lease"`
	Issue      NativeIssue            `json:"issue"`
	Discussion []NativeComment        `json:"discussion"`
	History    []CollaborationEvent   `json:"history"`
	Attempts   []NativeAttempt        `json:"attempts"`
	Change     *NativeChangeReference `json:"change,omitempty"`
}
