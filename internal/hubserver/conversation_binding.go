package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// firstDispatchableState reports the project's first non-terminal
// dispatchable workflow state, the lane a handoff issue starts in.
func firstDispatchableState(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if !state.Terminal && state.Dispatchable && !state.OperatorOnly {
			return state.Name, true
		}
	}
	return "", false
}

// reportFresh reports whether an observation is recent enough to act on.
func reportFresh(report providercapacity.Report, now time.Time) bool {
	return !report.ObservedAt.IsZero() && !now.Before(report.ObservedAt) && now.Before(report.ObservedAt.Add(providercapacity.MaxAge))
}

// conversationTranscriptRunes bounds each transcript line handed to a runner
// that cannot resume the provider thread.
const conversationTranscriptRunes = 2000

// conversationTranscriptMessages is how much history that runner receives.
const conversationTranscriptMessages = 20

func boundRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// conversationResumeDecision is the bind's answer to "does this attempt
// continue the recorded provider thread?", separated from the transcript read
// so the rule is testable on its own.
type conversationResumeDecision struct {
	// RunnerThreadID is the thread the binding runner named on this bind,
	// empty when it named none.
	RunnerThreadID string
	// RecordedRunnerID and RecordedOrigin describe the thread the
	// conversation carries.
	RecordedRunnerID string
	RecordedOrigin   string
	// BindingRunnerID is the runner holding the lease, empty for a
	// credential with no runner identity such as a legacy worker token.
	BindingRunnerID string
	// Coordinator reports that this bind is a coordinator turn rather than a
	// worker attempt on a linked issue.
	Coordinator bool
}

// resumeThread reports whether the binding attempt may continue the recorded
// provider thread (decisions section 9.3).
//
// A provider thread carries the instructions and the permission set of the
// turn that opened it. A coordinator thread was opened read-only, with no
// checkout and no authority to change files, run tests or move issue state;
// a worker thread was opened with all of them. Resuming across that boundary
// hands the resuming turn the wrong restrictions in one direction and the
// wrong authority in the other, so the origin must match the kind of turn
// that is binding.
//
// A runner that names the thread on this bind is opening its own, so the
// recorded origin does not apply to it. Otherwise the recorded runner must be
// this runner, and both identifiers must be present so that a credential with
// no runner identity never inherits another runner's thread.
func (d conversationResumeDecision) resumeThread() bool {
	if strings.TrimSpace(d.RunnerThreadID) != "" {
		return true
	}
	if d.RecordedRunnerID == "" || d.RecordedRunnerID != d.BindingRunnerID {
		return false
	}
	return d.RecordedOrigin == conversation.ThreadOriginFor(d.Coordinator)
}

// resumeFor decides what a binding runner continues from: its own provider
// thread, or the recent transcript when the thread was produced elsewhere or
// by the other kind of turn (decisions section 9.3). named reports that this
// bind supplied the thread. Messages already handed over as pending controls
// are left out so the runner does not see them twice.
func (c *conversationService) resumeFor(ctx context.Context, tx *sql.Tx, record conversationRecord, scope nativeScope, thread string, named bool, coordinator bool, pending []conversationControl) (conversationResumeResource, error) {
	decision := conversationResumeDecision{
		RecordedRunnerID: record.ProviderThreadRunnerID,
		RecordedOrigin:   record.ProviderThreadOrigin,
		BindingRunnerID:  scope.credential.Runner.RunnerID,
		Coordinator:      coordinator,
	}
	if named {
		decision.RunnerThreadID = thread
	}
	if decision.resumeThread() {
		return conversationResumeResource{ThreadID: thread, ThreadOrigin: conversation.ThreadOriginFor(coordinator)}, nil
	}
	handed := make(map[string]struct{}, len(pending))
	for _, control := range pending {
		handed[control.MessageID] = struct{}{}
	}
	messages, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? ORDER BY seq DESC LIMIT ?",
		record.ID, conversationTranscriptMessages+len(pending))
	if err != nil {
		return conversationResumeResource{}, err
	}
	transcript := make([]conversationTranscriptEntry, 0, conversationTranscriptMessages)
	for _, message := range messages {
		if len(transcript) >= conversationTranscriptMessages {
			break
		}
		if _, pendingControl := handed[message.ID]; pendingControl {
			continue
		}
		transcript = append(transcript, conversationTranscriptEntry{
			Role: message.Role, Kind: message.Kind, Text: boundRunes(message.Text, conversationTranscriptRunes),
		})
	}
	slices.Reverse(transcript)
	return conversationResumeResource{ThreadOrigin: conversation.ThreadOriginFor(coordinator), Transcript: transcript}, nil
}

// conversationResumeKind reports what the bind handed the runner, for the
// execution resource's resume field (decisions section 10.4).
func conversationResumeKind(resume conversationResumeResource) string {
	switch {
	case resume.ThreadID != "":
		return conversation.ResumeThread
	case len(resume.Transcript) > 0:
		return conversation.ResumeTranscript
	default:
		return ""
	}
}

// conversationTranscriptNotice is the exact copy the history records when a
// runner continues from a transcript instead of a resumable provider thread
// (decisions section 10.4).
func conversationTranscriptNotice(messages int) string {
	return fmt.Sprintf("Provider history was not available on this runner; continuing from a transcript of the last %d messages", messages)
}

// loadWorkerConversation resolves a conversation for a lease-fenced worker
// request and reports the linked work item whose lease the caller must hold.
func (c *conversationService) loadWorkerConversation(ctx context.Context, tx *sql.Tx, scope nativeScope, id string) (conversationRecord, string, error) {
	if err := conversation.ValidateConversationID(id); err != nil {
		return conversationRecord{}, "", nativeNotFound()
	}
	record, err := c.store.readConversation(ctx, tx, scope.organization, scope.project, id)
	if err != nil {
		return conversationRecord{}, "", translateConversationError(err)
	}
	if err := c.authorizeRead(scope, record); err != nil {
		return conversationRecord{}, "", err
	}
	return record, record.WorkItemID, nil
}
