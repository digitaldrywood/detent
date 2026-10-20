package tracker

import (
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

type ValidationAudit struct {
	Source      string                 `json:"source"`
	Coverage    string                 `json:"coverage"`
	Partial     bool                   `json:"partial"`
	Unavailable []string               `json:"unavailable"`
	Occurrences []ValidationOccurrence `json:"occurrences"`
}

type ValidationSource struct {
	Actor      Actor            `json:"actor"`
	Kind       string           `json:"kind"`
	WorkItemID NativeWorkItemID `json:"work_item_id"`
	RecordID   string           `json:"record_id"`
	Revision   Revision         `json:"revision,string"`
	ObservedAt time.Time        `json:"observed_at"`
}

type ValidationOccurrence struct {
	Source      ValidationSource       `json:"source"`
	Scheduled   gate.ScheduledEvidence `json:"scheduled"`
	Comparisons []ValidationComparison `json:"comparisons"`
}

type ValidationComparison struct {
	gate.CheckComparison
	CheckIndex        *int                `json:"scheduled_check_index,omitempty"`
	WorkItemID        NativeWorkItemID    `json:"work_item_id,omitempty"`
	ChangeID          string              `json:"change_id,omitempty"`
	VersionID         string              `json:"version_id,omitempty"`
	VersionRunID      string              `json:"version_run_id,omitempty"`
	NativeRunID       string              `json:"native_run_id,omitempty"`
	VersionAttemptID  string              `json:"version_attempt_id,omitempty"`
	NativeAttemptID   string              `json:"native_attempt_id,omitempty"`
	LocalAttemptID    int64               `json:"local_attempt_id,omitempty"`
	ReviewedHeadSHA   string              `json:"reviewed_head_sha,omitempty"`
	IntegratedHeadSHA string              `json:"integrated_head_sha,omitempty"`
	IntegratedSource  string              `json:"integrated_source"`
	Attribution       string              `json:"attribution"`
	Local             *gate.CommandResult `json:"local,omitempty"`
	ObservedAt        time.Time           `json:"observed_at,omitzero"`
}

func PublicValidationCommand(command string) string {
	text := NativeFinalization{}
	return text.publicText(command)
}
