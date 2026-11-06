package conversationstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Conversation struct {
	OrganizationID         tracker.OrganizationID
	ProjectID              tracker.ProjectID
	ID                     string
	CreationKey            string
	CreationHash           string
	OwnerPrincipalID       string
	OwnerSubject           string
	Origin                 string
	Title                  string
	Visibility             conversation.Visibility
	Status                 conversation.Status
	SubjectWorkItemID      string
	WorkItemID             string
	LinkedAt               *time.Time
	Revision               int64
	ProviderThreadID       string
	ProviderThreadRunnerID string
	ProviderThreadOrigin   string
	Execution              conversation.Execution
	EventSequence          int64
	Preferences            conversation.Preferences
	ReadAudience           string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	LastMessageAt          *time.Time
	SettledAt              *time.Time
	Archived               bool
	ContentReference       string
	MessageCount           int64
}

type Message struct {
	OrganizationID   tracker.OrganizationID
	ConversationID   string
	ID               string
	Sequence         int64
	Role             conversation.Role
	Kind             conversation.MessageKind
	Text             string
	Data             json.RawMessage
	Delivery         conversation.Delivery
	AttemptID        string
	ThreadID         string
	TurnID           string
	ProviderItemID   string
	Actor            conversation.Actor
	CommandKey       string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ContentReference string
	Revision         int64
	TextBytes        int64
	StoredTextBytes  int64
	ToolName         *string
	ToolItemID       *string
	ToolStatus       *string
	OperatorActionID *string
	OperatorAgent    *string
	OperatorResult   *string
}

type Question struct {
	OrganizationID tracker.OrganizationID
	ConversationID string
	conversation.Question
	RequestID        string
	ContentReference string
}

type QuestionPage struct {
	Items []Question
	Next  string
}

type Command struct {
	OrganizationID   tracker.OrganizationID
	ConversationID   string
	Key              string
	Kind             conversation.CommandKind
	RequestHash      string
	Receipt          conversation.Receipt
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ContentReference string
}

type Event struct {
	OrganizationID tracker.OrganizationID
	ConversationID string
	conversation.Event
	ContentReference string
}

type MessageReference struct {
	OrganizationID tracker.OrganizationID
	ConversationID string
	MessageID      string
	Position       int64
	Kind           conversation.ReferenceKind
	TargetID       string
	Label          string
	CreatedAt      time.Time
}

type ToolRevision struct {
	OrganizationID   tracker.OrganizationID
	ConversationID   string
	MessageID        string
	Revision         int64
	Text             string
	Tool             string
	ItemID           string
	Status           string
	ContentReference string
}

type ContentObject struct {
	OrganizationID tracker.OrganizationID
	ID             string
	ArchiveID      string
	Reference      attachment.ArchiveReference
	CreatedAt      time.Time
}

type Attachment struct {
	OrganizationID tracker.OrganizationID
	ConversationID string
	ProjectID      tracker.ProjectID
	ID             string
	MessageID      string
	PrincipalID    string
	Name           string
	MIME           string
	Size           int64
	ArtifactRef    string
	ContentID      string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	DeletedAt      *time.Time
}

type ConversationQuery struct {
	ProjectID         tracker.ProjectID
	ProjectIDs        []tracker.ProjectID
	PrincipalID       string
	ReadAudience      string
	Cursor            string
	Limit             int
	Settled           *bool
	Title             string
	SubjectWorkItemID string
}

type ConversationPage struct {
	Items []Conversation
	Next  string
}

type LifecycleKind string

const (
	LifecycleRestart   LifecycleKind = "restart"
	LifecycleExecution LifecycleKind = "execution"
	LifecycleSettle    LifecycleKind = "settle"
)

type LifecycleQuery struct {
	Kind    LifecycleKind
	AfterID string
	Before  time.Time
	Limit   int
}

type PendingConversation struct {
	ID     string
	Linked bool
}

type PendingConversationPage struct {
	Items []PendingConversation
	Next  string
}

type MessagePage struct {
	Items []Message
	Next  int64
}

type MessageFilter struct {
	OperatorActionID string
	OperatorResult   string
	Oldest           bool
	ConversationID   string
	AttemptID        string
	TurnID           string
	Role             conversation.Role
	Kind             conversation.MessageKind
	Deliveries       []conversation.Delivery
	BeforeSequence   int64
	AfterSequence    int64
	Limit            int
}

type EventPage struct {
	Items []Event
	Next  int64
}

type CommandPage struct {
	Items []Command
	Next  string
}

type Reader interface {
	Conversation(context.Context, tracker.ProjectID, string) (Conversation, error)
	ConversationByID(context.Context, string) (Conversation, error)
	ConversationByExecutionLease(context.Context, string) (Conversation, error)
	ConversationByWorkItem(context.Context, tracker.ProjectID, string) (Conversation, error)
	ConversationByCreationKey(context.Context, string, string) (Conversation, error)
	Conversations(context.Context, ConversationQuery) (ConversationPage, error)
	LifecycleConversations(context.Context, LifecycleQuery) (ConversationPage, error)
	PendingConversations(context.Context, string, int) (PendingConversationPage, error)
	Messages(context.Context, string, int64, int) (MessagePage, error)
	FilteredMessages(context.Context, MessageFilter) (MessagePage, error)
	Message(context.Context, string, string) (Message, error)
	MessageByCommandKey(context.Context, string, string) (Message, bool, error)
	ConversationEvents(context.Context, string, int64, int) (EventPage, error)
	Question(context.Context, string, string) (Question, error)
	Questions(context.Context, string, string, int) ([]Question, error)
	OutstandingQuestions(context.Context, string, string, string, string, int) (QuestionPage, error)
	RestartQuestions(context.Context, string, string, int) (QuestionPage, error)
	PendingCommands(context.Context, string, string, int) (CommandPage, error)
	Command(context.Context, string, string) (Command, bool, error)
	TurnBatch(context.Context, string, string) (int64, bool, error)
	HasConversationStart(context.Context, string) (bool, error)
	MessageReferences(context.Context, string, string) ([]MessageReference, error)
	ToolRevisions(context.Context, string, string, int) ([]ToolRevision, error)
	PendingUserMessages(context.Context, string) (int, error)
	ConversationContent(context.Context, string) (ContentObject, error)
}

type Writer interface {
	Reader
	LockConversation(context.Context, tracker.ProjectID, string) (Conversation, error)
	CreateConversation(context.Context, Conversation) (Conversation, error)
	UpdateConversation(context.Context, Conversation, int64) (Conversation, error)
	CreateMessage(context.Context, Message) (Message, error)
	UpdateMessage(context.Context, Message, int64) (Message, error)
	UpsertQuestion(context.Context, Question) error
	ReserveCommand(context.Context, Command) (Command, bool, error)
	UpdateCommand(context.Context, Command) error
	AppendConversationEvent(context.Context, Event) (Event, error)
	RecordStart(context.Context, string, string, time.Time) error
	RecordAudience(context.Context, string, string, conversation.Visibility, conversation.Visibility, time.Time) error
	RecordTurnBatch(context.Context, string, string, int64, time.Time) error
	ReplaceMessageReferences(context.Context, string, string, []MessageReference, time.Time) error
	PutToolRevision(context.Context, ToolRevision) error
	PutConversationContent(context.Context, ContentObject) error
}

type Repository interface {
	Read(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	Write(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type AttentionReader interface {
	CoordinatorAttentionItems(context.Context, []tracker.ProjectID, int) ([]corestore.WorkItemReference, error)
}
