package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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

// resumeFor decides what a binding runner continues from: its own provider
// thread, or the recent transcript when the thread was produced elsewhere or
// by the other kind of turn. named reports that this
// bind supplied the thread. Messages already handed over as pending controls
// are left out so the runner does not see them twice.
func (c *conversationService) resumeFor(ctx context.Context, tx nativeQueryer, record conversationRecord, scope nativeScope, thread string, named bool, coordinator bool, pending []conversationControl) (conversationResumeResource, error) {
	decision := conversationResumeDecision{
		RecordedRunnerID: record.ProviderThreadRunnerID,
		RecordedOrigin:   record.ProviderThreadOrigin,
		BindingRunnerID:  scope.credential.Runner.RunnerID,
		Coordinator:      coordinator,
	}
	if named {
		decision.RunnerThreadID = thread
	}
	if decision.ResumeThread() {
		return conversationResumeResource{ThreadID: thread, ThreadOrigin: conversation.ThreadOriginFor(coordinator)}, nil
	}
	if record.ReadAudience != "" && !coordinator {
		return conversationResumeResource{}, nil
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
// execution resource's resume field.
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
// runner continues from a transcript instead of a resumable provider thread.
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
	readable := record
	if scope.credential.Scope == apiScopeWorker {
		readable.ReadAudience = ""
	}
	if err := c.authorizeRead(ctx, tx, scope, readable); err != nil {
		return conversationRecord{}, "", err
	}
	return record, record.WorkItemID, nil
}
