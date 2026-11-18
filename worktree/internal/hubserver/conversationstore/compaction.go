package conversationstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const CompactionBatch = 500
const CompactionConversations = 4
const EventRetention = 30 * 24 * time.Hour

type CompactionState struct {
	ThroughSequence        int64
	ScannedSequence        int64
	ContentScannedSequence int64
	ContentReference       string
}

type CompactionCandidate struct {
	Conversation
	Cursor string
}

type CompactionPage struct {
	Items []CompactionCandidate
	Next  string
}

type CompactionRequest struct {
	Conversation Conversation
	Sequences    []int64
	State        CompactionState
	Before       time.Time
	Now          time.Time
	Full         bool
}

type CompactionEventPage struct {
	Items           []Event
	ScannedSequence int64
	ScannedCount    int
	Complete        bool
}

type CompactionReader interface {
	CompactionCandidates(context.Context, string, time.Time, time.Time, int) (CompactionPage, error)
	ConversationFullCompactionEligible(context.Context, tracker.ProjectID, string, time.Time, time.Time) (bool, error)
	UnprojectedConversationEvents(context.Context, string, int) (EventPage, error)
	ObsoleteConversationEvents(context.Context, string, int64, int64, bool, int) (CompactionEventPage, error)
}

type CompactionWriter interface {
	CompactionReader
	ProjectConversationEvents(context.Context, string, []Event) error
	CompactConversationEvents(context.Context, tracker.ProjectID, CompactionRequest) (bool, error)
}
