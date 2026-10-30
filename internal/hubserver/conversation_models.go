package hubserver

import (
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

type conversationAttachmentRecord = hubconversation.AttachmentRecord

type conversationResumeDecision = hubconversation.ResumeDecision

type conversationExecutionResource = hubconversation.ExecutionResource

type conversationWorkItemResource = hubconversation.WorkItemResource

type conversationResource = hubconversation.Resource

type conversationMessageResource = hubconversation.MessageResource

type conversationReferenceResource = hubconversation.ReferenceResource

type conversationWorkItem = hubconversation.WorkItem

type conversationRecord = hubconversation.Record

type conversationMessageRecord = hubconversation.MessageRecord

type conversationMessageReference = hubconversation.MessageReference

type conversationQuestionRecord = hubconversation.QuestionRecord

func projectAttachments(records []conversationAttachmentRecord) []conversation.Attachment {
	return hubconversation.ProjectAttachments(records)
}

func conversationOptional(value string) *string {
	return hubconversation.Optional(value)
}

func conversationBetweenTurns(execution conversation.Execution) bool {
	return hubconversation.BetweenTurns(execution)
}

func projectExecution(execution conversation.Execution) conversationExecutionResource {
	return hubconversation.ProjectExecution(execution)
}

func conversationRunnerBound(execution conversation.Execution) bool {
	return hubconversation.RunnerBound(execution)
}

func conversationIssueResult(record conversationRecord) map[string]any {
	return hubconversation.IssueResult(record)
}

func projectConversation(record conversationRecord) conversationResource {
	return hubconversation.ProjectConversation(record)
}

func projectMessage(record conversationMessageRecord) conversationMessageResource {
	return hubconversation.ProjectMessage(record)
}
