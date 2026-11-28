package checkpoint

import (
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DeferredRunCheckpoint struct {
	Handoff tracker.NativeCheckpoint    `json:"handoff"`
	Diff    *tracker.AttemptDiffRequest `json:"diff,omitempty"`
}

type State struct {
	Settled                  bool                           `json:"-"`
	MergeResolution          bool                           `json:"merge_resolution,omitempty"`
	Observations             []tracker.NativeRunEvent       `json:"observations,omitempty"`
	Publication              *tracker.NativePRPublication   `json:"publication,omitempty"`
	Lease                    tracker.NativeLease            `json:"lease"`
	Data                     tracker.NativeRunData          `json:"data"`
	Rejected                 *tracker.NativeRunEvent        `json:"rejected,omitempty"`
	Pending                  *tracker.NativeRunEvent        `json:"pending,omitempty"`
	PendingCheckpointDiff    *tracker.AttemptDiffRequest    `json:"pending_checkpoint_diff,omitempty"`
	DeferredCheckpoint       *DeferredRunCheckpoint         `json:"deferred_checkpoint,omitempty"`
	Diff                     *tracker.AttemptDiffRequest    `json:"diff,omitempty"`
	DiffCommitBound          bool                           `json:"diff_commit_bound,omitempty"`
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
	PreparedRoundVerdict     *progresscheck.RoundInput      `json:"prepared_round_verdict,omitempty"`
}

func (s State) RestoredDiff(lease tracker.NativeLease) *tracker.AttemptDiffRequest {
	if s.Diff == nil || !s.DiffCommitBound && !s.Diff.CommitBound {
		return s.Diff
	}
	diff := *s.Diff
	diff.CommitBound = false
	if lease.ID == "" || lease.FencingToken <= 0 || !SameLease(s.Lease, lease) || s.Data.LeaseID != lease.ID ||
		s.Data.FencingToken != lease.FencingToken || s.Data.PolicyID != lease.PolicyID {
		return &diff
	}
	handoff := s.Data.Handoff
	if s.Pending != nil && s.Pending.Data.Handoff != nil {
		handoff = s.Pending.Data.Handoff
	}
	if s.DeferredCheckpoint != nil {
		handoff = &s.DeferredCheckpoint.Handoff
	}
	if handoff == nil || handoff.HeadSHA != s.WorktreeHead || handoff.WorktreeState != s.WorktreeState ||
		s.WorktreeState != "clean" && s.WorktreeState != "unpushed" ||
		s.Diff.HeadSHA == "" || s.Diff.HeadSHA != handoff.HeadSHA || s.Diff.BaseSHA == "" || handoff.BaseSHA != "" && handoff.BaseSHA != s.Diff.BaseSHA {
		return &diff
	}
	diff.CommitBound = true
	return &diff
}
