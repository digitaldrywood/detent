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
	DeliveryID                                         string
	Attempts                                           int64
	Email, Title, Token, OrganizationSlug, ProjectSlug string
	Number                                             int
	Notices                                            []IssueNotice
}

type NotificationReceipt struct {
	ID                       string
	WorkItemID               tracker.NativeWorkItemID
	UserID, Status           string
	Attempts                 int64
	NoticeCount              int
	ClaimedAt, NextAttemptAt time.Time
	FinishedAt               *time.Time
}

type NotificationCollector interface {
	NotificationEvents(context.Context, tracker.OrganizationID) ([]eventstore.Event, error)
	QueueNotification(context.Context, tracker.OrganizationID, eventstore.Event, IssueNotice, tracker.NativeWorkItemID, bool, int) (bool, int, error)
}

type NotificationDelivery interface {
	ClaimNotification(context.Context, tracker.OrganizationID, time.Time) (IssueNotification, bool, error)
	UnsubscribeNotification(context.Context, tracker.OrganizationID, string) error
	FinishNotification(context.Context, tracker.OrganizationID, string, int64, string, time.Time, time.Time) error
	NotificationReceipt(context.Context, tracker.OrganizationID, string) (NotificationReceipt, error)
}

type NotificationRepository interface {
	NotificationCollector
	NotificationDelivery
}
