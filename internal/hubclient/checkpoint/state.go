package checkpoint

import (
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type State struct {
	Settled                  bool                           `json:"-"`
	MergeResolution          bool                           `json:"merge_resolution,omitempty"`
	Observations             []tracker.NativeRunEvent       `json:"observations,omitempty"`
	Publication              *tracker.NativePRPublication   `json:"publication,omitempty"`
	Lease                    tracker.NativeLease            `json:"lease"`
	Data                     tracker.NativeRunData          `json:"data"`
	Rejected                 *tracker.NativeRunEvent        `json:"rejected,omitempty"`
	Pending                  *tracker.NativeRunEvent        `json:"pending,omitempty"`
	Diff                     *tracker.AttemptDiffRequest    `json:"diff,omitempty"`
	StoredSeq                int64                          `json:"stored_seq"`
	WorktreeState            string                         `json:"worktree_state"`
	WorktreeHead             string                         `json:"worktree_head"`
	Repository               string                         `json:"repository"`
	SourceRequired           bool                           `json:"source_required,omitempty"`
	RetainedSource           *tracker.ChangeSourceCapture   `json:"retained_source,omitempty"`
	RecoveredSource          *tracker.NativeChangeReference `json:"recovered_source,omitempty"`
	ConversationContinuation bool                           `json:"conversation_continuation"`
	PreparedOutcome          string                         `json:"prepared_outcome"`
	PreparedMessage          string                         `json:"prepared_message"`
	PreparedDisposition      *tracker.NativeDisposition     `json:"prepared_disposition,omitempty"`
	PreparedFinalization     *tracker.NativeFinalization    `json:"prepared_finalization,omitempty"`
	PreparedLanding          *runner.NativeLanding          `json:"prepared_landing,omitempty"`
	PreparedFailure          *tracker.NativeTerminalFailure `json:"prepared_failure,omitempty"`
}
