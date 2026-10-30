package hubconversation

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// AttachmentRecord is one stored attachment. MessageID is empty
// while the upload is unsent; ExpiresAt is set only then.
type AttachmentRecord struct {
	ID             string
	ContentID      string
	ConversationID string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	MessageID      string
	PrincipalID    string
	Name           string
	MIME           string
	Size           int64
	ArtifactRef    string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	DeletedAt      *time.Time
}

// attachmentURL is the hub path that streams an attachment. It is
// the `url` every attachment resource carries, so the client and the runner
// both read the blob through the conversation's audience.
func attachmentURL(organization tracker.OrganizationID, project tracker.ProjectID, conversationID, attachmentID string) string {
	return "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) +
		"/conversations/" + conversationID + "/attachments/" + attachmentID
}

func (r AttachmentRecord) Resource() conversation.Attachment {
	return conversation.Attachment{
		ID: r.ID, Name: r.Name, MIME: r.MIME, Size: r.Size,
		URL: attachmentURL(r.OrganizationID, r.ProjectID, r.ConversationID, r.ID),
	}
}

func ProjectAttachments(records []AttachmentRecord) []conversation.Attachment {
	projected := make([]conversation.Attachment, 0, len(records))
	for _, record := range records {
		projected = append(projected, record.Resource())
	}
	return projected
}

// ResumeDecision is the bind's answer to "does this attempt
// continue the recorded provider thread?", separated from the transcript read
// so the rule is testable on its own.
type ResumeDecision struct {
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

// ResumeThread reports whether the binding attempt may continue the recorded
// provider thread.
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
func (d ResumeDecision) ResumeThread() bool {
	if strings.TrimSpace(d.RunnerThreadID) != "" {
		return true
	}
	if d.RecordedRunnerID == "" || d.RecordedRunnerID != d.BindingRunnerID {
		return false
	}
	return d.RecordedOrigin == conversation.ThreadOriginFor(d.Coordinator)
}

type ownerResource struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
}

type ExecutionResource struct {
	Status conversation.ExecutionStatus `json:"status"`
	// The owner identifiers are null rather than empty when no attempt owns
	// the conversation; the client tells "absent" from "known" by null.
	AttemptID *string `json:"attempt_id"`
	RunID     *string `json:"run_id"`
	RunnerID  *string `json:"runner_id"`
	ThreadID  *string `json:"thread_id"`
	TurnID    *string `json:"turn_id"`
	// Resume reports what the bound runner continued from: its own provider
	// thread, a transcript, or nothing.
	Resume       string                    `json:"resume"`
	Capabilities conversation.Capabilities `json:"capabilities"`
	Error        *string                   `json:"error"`
	UpdatedAt    time.Time                 `json:"updated_at"`
}

// WorkItemResource is the linked issue as the client renders it:
// enough to show and open the issue card without a second request.
type WorkItemResource struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Lane       string `json:"lane"`
	// RunnerBound reports that an attempt currently owns the issue, which
	// is what the client shows instead of "waiting for runner".
	RunnerBound bool `json:"runner_bound"`
}

type Resource struct {
	ID             string                  `json:"id"`
	WebURL         string                  `json:"web_url,omitempty"`
	OrganizationID tracker.OrganizationID  `json:"organization_id"`
	ProjectID      tracker.ProjectID       `json:"project_id"`
	Title          string                  `json:"title"`
	Visibility     conversation.Visibility `json:"visibility"`
	Origin         string                  `json:"origin"`
	Status         conversation.Status     `json:"status"`
	Archived       bool                    `json:"archived"`
	// Preferences are the conversation's turn preferences; each field is
	// "auto" or an explicit value.
	Preferences       conversation.Preferences `json:"preferences"`
	SubjectWorkItemID *string                  `json:"subject_work_item_id,omitempty"`
	WorkItemID        *string                  `json:"work_item_id"`
	LinkedAt          *time.Time               `json:"linked_at"`
	WorkItem          *WorkItemResource        `json:"work_item"`
	Owner             ownerResource            `json:"owner"`
	Execution         ExecutionResource        `json:"execution"`
	Revision          int64                    `json:"revision"`
	EventSeq          int64                    `json:"event_seq"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
	LastMessageAt     *time.Time               `json:"last_message_at"`
	// MessageCount is the whole history, so the share confirmation can say
	// how much becomes readable.
	MessageCount int64 `json:"message_count"`
}

type MessageResource struct {
	Revision       int64                    `json:"-"`
	ContentID      string                   `json:"-"`
	ID             string                   `json:"id"`
	ConversationID string                   `json:"conversation_id"`
	Seq            int64                    `json:"seq"`
	Role           conversation.Role        `json:"role"`
	Kind           conversation.MessageKind `json:"kind"`
	Text           string                   `json:"text"`
	Data           any                      `json:"data"`
	Delivery       conversation.Delivery    `json:"delivery"`
	AttemptID      *string                  `json:"attempt_id"`
	TurnID         *string                  `json:"turn_id"`
	ProviderItemID *string                  `json:"provider_item_id"`
	Actor          conversation.Actor       `json:"actor"`
	CommandKey     *string                  `json:"command_key"`
	// References are the issues and conversations the text names, resolved
	// against the project.
	References []ReferenceResource `json:"references"`
	// Attachments are the uploads the message carries, each with the URL
	// that streams it.
	Attachments []conversation.Attachment `json:"attachments"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

// ReferenceResource is one resolved reference as the client
// renders it: a link with a label.
type ReferenceResource struct {
	Kind  conversation.ReferenceKind `json:"kind"`
	ID    string                     `json:"id"`
	Label string                     `json:"label"`
	URL   string                     `json:"url"`
}

func projectReferences(references []MessageReference) []ReferenceResource {
	projected := make([]ReferenceResource, 0, len(references))
	for _, reference := range references {
		projected = append(projected, ReferenceResource{
			Kind: reference.Kind, ID: reference.ID, Label: reference.Label,
			URL: reference.URL,
		})
	}
	return projected
}

func Optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func BetweenTurns(execution conversation.Execution) bool {
	return execution.Status == conversation.ExecutionRunning && execution.Owner.AttemptID != "" && execution.Owner.TurnID == ""
}

func ProjectExecution(execution conversation.Execution) ExecutionResource {
	status := execution.Status
	if status == "" {
		status = conversation.ExecutionIdle
	}
	capabilities := execution.Capabilities
	if BetweenTurns(execution) {
		capabilities.Steer = false
	}
	return ExecutionResource{
		Status:       status,
		AttemptID:    Optional(execution.Owner.AttemptID),
		RunID:        Optional(execution.Owner.RunID),
		RunnerID:     Optional(execution.Owner.RunnerID),
		ThreadID:     Optional(execution.Owner.ThreadID),
		TurnID:       Optional(execution.Owner.TurnID),
		Resume:       execution.Resume,
		Capabilities: capabilities,
		Error:        Optional(execution.Error),
		UpdatedAt:    execution.UpdatedAt,
	}
}

// RunnerBound reports whether an attempt currently owns the
// conversation: an attempt identifier that has not reached a terminal
// execution status.
func RunnerBound(execution conversation.Execution) bool {
	return execution.Owner.AttemptID != "" && !execution.Status.Terminal()
}

// projectWorkItem renders the linked issue summary. An unlinked conversation,
// or one whose issue could not be resolved, projects null.
func projectWorkItem(record Record) *WorkItemResource {
	if record.WorkItemID == "" || record.WorkItem == nil {
		return nil
	}
	return &WorkItemResource{
		ID:          record.WorkItem.ID,
		Identifier:  record.WorkItem.Identifier,
		Title:       record.WorkItem.Title,
		Lane:        record.WorkItem.Lane,
		RunnerBound: RunnerBound(record.Execution),
	}
}

// IssueResult is the data.issue payload of the status message a
// completed handoff appends. It survives a reload and reaches other tabs,
// so the result card does not depend on the linking response.
func IssueResult(record Record) map[string]any {
	item := projectWorkItem(record)
	if item == nil {
		return nil
	}
	return map[string]any{
		"id": item.ID, "identifier": item.Identifier, "title": item.Title,
		"state": item.Lane, "lane": item.Lane, "runner_bound": item.RunnerBound,
	}
}

// WorkItem is the linked issue summary cached on a conversation
// record so that every projection reports the same identity without a
// second lookup. runner_bound is not cached: it comes from the record's own
// execution state, which changes far more often than the issue.
type WorkItem struct {
	ID         string
	Identifier string
	Title      string
	Lane       string
}

func ProjectConversation(record Record) Resource {
	return Resource{
		ID:                record.ID,
		OrganizationID:    record.OrganizationID,
		ProjectID:         record.ProjectID,
		Title:             record.Title,
		Visibility:        record.Visibility,
		Origin:            record.Origin,
		Status:            record.Status,
		Archived:          record.Archived,
		Preferences:       record.Preferences.Normalized(),
		SubjectWorkItemID: Optional(record.SubjectWorkItemID),
		WorkItemID:        Optional(record.WorkItemID),
		LinkedAt:          record.LinkedAt,
		WorkItem:          projectWorkItem(record),
		Owner:             ownerResource{PrincipalID: record.OwnerPrincipalID, Subject: record.OwnerSubject},
		Execution:         ProjectExecution(record.Execution),
		Revision:          record.Revision,
		EventSeq:          record.EventSeq,
		CreatedAt:         record.CreatedAt,
		UpdatedAt:         record.UpdatedAt,
		LastMessageAt:     record.LastMessageAt,
		MessageCount:      record.MessageCount,
	}
}

func ProjectMessage(record MessageRecord) MessageResource {
	var data any = map[string]any{}
	if len(record.Data) > 0 && string(record.Data) != "null" {
		data = record.Data
	}
	return MessageResource{
		ContentID:      record.ContentID,
		Revision:       record.Revision,
		ID:             record.ID,
		ConversationID: record.ConversationID,
		Seq:            record.Seq,
		Role:           record.Role,
		Kind:           record.Kind,
		Text:           record.Text,
		Data:           data,
		Delivery:       record.Delivery,
		AttemptID:      Optional(record.AttemptID),
		TurnID:         Optional(record.TurnID),
		ProviderItemID: Optional(record.ProviderItemID),
		Actor:          record.Actor,
		CommandKey:     Optional(record.CommandKey),
		References:     projectReferences(record.References),
		Attachments:    ProjectAttachments(record.Attachments),
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
	}
}

type Record struct {
	ContentID         string
	ID                string
	ReadAudience      string
	OrganizationID    tracker.OrganizationID
	ProjectID         tracker.ProjectID
	OwnerPrincipalID  string
	OwnerSubject      string
	Origin            string
	Title             string
	Visibility        conversation.Visibility
	Status            conversation.Status
	Archived          bool
	SubjectWorkItemID string
	WorkItemID        string
	LinkedAt          *time.Time
	// WorkItem caches the linked issue summary the projections report. It
	// is filled by resolveConversationWorkItems on every read.
	WorkItem         *WorkItem
	Revision         int64
	ProviderThreadID string
	// ProviderThreadRunnerID names the runner whose provider login produced
	// ProviderThreadID. It is empty for the transitional hub-side
	// coordinator, so no runner resumes a thread it cannot reach.
	ProviderThreadRunnerID string
	// ProviderThreadOrigin names the kind of turn that produced
	// ProviderThreadID: conversation.ThreadOriginCoordinator or
	// conversation.ThreadOriginWorker. A thread carries the instructions and
	// the permission set of the turn that opened it, so only a turn of the
	// same kind resumes it. It is empty for threads
	// recorded before the column existed, which therefore never resume.
	ProviderThreadOrigin string
	Execution            conversation.Execution
	EventSeq             int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
	LastMessageAt        *time.Time
	// SettledAt is when the conversation last became settled, nil while it
	// is active.
	SettledAt *time.Time
	// Preferences are the conversation's turn preferences. Every field is
	// "auto" until the user overrides it.
	Preferences conversation.Preferences
	// MessageCount is the conversation's whole history, filled by every
	// read so the audience preview can report it.
	MessageCount int64
}

type MessageRecord struct {
	Revision        int64
	ID              string
	ContentID       string
	ContentHash     string `json:"-"`
	TextBytes       int64
	StoredTextBytes sql.NullInt64
	ConversationID  string
	Seq             int64
	Role            conversation.Role
	Kind            conversation.MessageKind
	Text            string
	Data            json.RawMessage
	Delivery        conversation.Delivery
	AttemptID       string
	ThreadID        string
	TurnID          string
	ProviderItemID  string
	Actor           conversation.Actor
	CommandKey      string
	// References are the issues and conversations the message names, filled
	// by every read that projects the message.
	References []MessageReference
	// Attachments are the uploads bound to the message, filled by the same
	// reads as References.
	Attachments []AttachmentRecord
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// MessageReference is one stored, resolved reference: the hub
// only records targets it could resolve, so a row always names something
// that exists.
type MessageReference struct {
	URL   string
	Kind  conversation.ReferenceKind
	ID    string
	Label string
}

type QuestionRecord struct {
	ContentID string
	conversation.Question
	RequestID string
}
