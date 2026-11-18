package accountstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type IssueNotice struct {
	StateFrom string `json:"state_from,omitempty"`
	StateTo   string `json:"state_to,omitempty"`
	Author    string `json:"author,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Related   bool   `json:"related,omitempty"`
	Verify    bool   `json:"verify,omitempty"`
}

type IssueNotification struct {
	Email, Title, Token, OrganizationSlug, ProjectSlug string
	Number                                             int
	Notices                                            []IssueNotice
}

type NotificationCandidate struct {
	ProjectID  tracker.ProjectID
	WorkItemID tracker.NativeWorkItemID
	UserID     string
	DueAt      time.Time
	Revision   int64
	Verify     bool
}

type NotificationCursor struct {
	DueAt      time.Time
	WorkItemID tracker.NativeWorkItemID
	UserID     string
	Revision   int64
}

type NotificationPage struct {
	Candidate NotificationCandidate
	Next      NotificationCursor
	Found     bool
	More      bool
}

type NotificationCollector interface {
	NotificationEvents(context.Context, tracker.OrganizationID) ([]eventstore.Event, error)
	QueueNotification(context.Context, tracker.OrganizationID, eventstore.Event, IssueNotice, tracker.NativeWorkItemID, bool, int) (bool, int, error)
}

type NotificationDelivery interface {
	NotificationSeek(context.Context, tracker.OrganizationID) (NotificationCursor, error)
	AdvanceNotificationSeek(context.Context, tracker.OrganizationID, NotificationCursor) (bool, error)
	DueNotification(context.Context, tracker.OrganizationID, time.Time, NotificationCursor, []NotificationCandidate) (NotificationPage, error)
	ClaimNotification(context.Context, tracker.OrganizationID, time.Time, NotificationCandidate, bool) (IssueNotification, NotificationCandidate, bool, error)
	UnsubscribeNotification(context.Context, tracker.OrganizationID, string) error
}

type NotificationRepository interface {
	NotificationCollector
	NotificationDelivery
}
