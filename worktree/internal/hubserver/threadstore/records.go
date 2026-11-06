package threadstore

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reference struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ProjectID string `json:"project_id,omitempty"`
	IssueID   string `json:"issue_id,omitempty"`
	Label     string `json:"label,omitempty"`
}

type Participant struct {
	PrincipalID string `json:"principal_id"`
	Name        string `json:"name"`
	Client      string `json:"client,omitempty"`
}

type Thread struct {
	ID            string        `json:"id"`
	Scope         string        `json:"scope"`
	ProjectID     string        `json:"project_id,omitempty"`
	IssueID       string        `json:"issue_id,omitempty"`
	IssueNumber   int           `json:"issue_number,omitempty"`
	Title         string        `json:"title"`
	Status        string        `json:"status"`
	Creator       string        `json:"creator_principal"`
	CreatorName   string        `json:"creator_name"`
	CreatorClient string        `json:"creator_client,omitempty"`
	PostCount     int64         `json:"post_count"`
	Unread        int64         `json:"unread_count"`
	Preview       string        `json:"last_post_preview"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	Participants  []Participant `json:"participants"`
	References    []Reference   `json:"references"`
}

type Post struct {
	Sequence   int64       `json:"sequence"`
	Author     string      `json:"author_principal"`
	Name       string      `json:"author_name"`
	ActingFor  string      `json:"acting_for,omitempty"`
	Client     string      `json:"client_name,omitempty"`
	Kind       string      `json:"kind"`
	Body       string      `json:"body"`
	CreatedAt  string      `json:"created_at"`
	References []Reference `json:"references"`
}

type Page struct {
	Threads    []Thread `json:"threads"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type Read struct {
	Thread  Thread `json:"thread"`
	Posts   []Post `json:"posts"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
}

var ErrIdempotencyConflict = errors.New("thread request key was already used")

type Record struct {
	Thread
	RequestKey string
	InputHash  string
}
type PostRecord struct {
	BodyReference attachment.ArchiveReference
	BodyArchiveID string
	Post
	RequestKey string
	InputHash  string
}
type Filter struct {
	Principal   string
	Reporter    string
	AllProjects bool
	Projects    []tracker.ProjectID
	Scope       string
	ProjectID   tracker.ProjectID
	IssueID     string
	Status      string
	Search      string
	BeforeTime  string
	BeforeID    string
	Limit       int
}
type Cursor struct {
	Principal    string
	LastSequence int64
}
type Target struct {
	Reference
	Uploader string
}
type Import struct {
	Thread Record
	Posts  []PostRecord
	Reads  []Cursor
}
type Reader interface {
	ThreadUnreadJob(context.Context) (UnreadJob, error)
	ThreadUnreadPage(context.Context, UnreadJob, int) ([]UnreadRow, error)
	Thread(context.Context, string, Filter) (Record, error)
	Threads(context.Context, Filter) ([]Record, error)
	ThreadByRequest(context.Context, string, string) (Record, error)
	ThreadParticipants(context.Context, string) ([]Participant, error)
	ThreadReferences(context.Context, string, int64) ([]Reference, error)
	ThreadReferenceTarget(context.Context, string, string) (Target, error)
	ThreadPosts(context.Context, string, int64, int64, int) ([]PostRecord, error)
	ThreadPostByRequest(context.Context, string, string, string) (PostRecord, error)
	ThreadReadCursor(context.Context, string, string) (int64, error)
}
type Writer interface {
	RequestThreadUnread(context.Context, UnreadRequest) (Unread, error)
	ApplyThreadUnread(context.Context, UnreadJob, bool) error
	Reader
	CreateThread(context.Context, Record) error
	AppendThreadPost(context.Context, string, PostRecord) (PostRecord, error)
	ResolveThread(context.Context, string, string) error
	AdvanceThreadRead(context.Context, string, string, int64) error
}

type Unread struct {
	Count   *int64
	Pending bool
}
type UnreadRequest struct {
	ID                     string
	Principal              string
	CredentialID           string
	UserID                 string
	MemberPrincipal        string
	KeyKind                string
	RequireCredentialGrant bool
	Reporter               string
	ScopeHash              string
	Projects               []tracker.ProjectID
	Refresh                bool
}
type UnreadJob struct {
	UnreadRequest
	Requested     int64
	Completed     int64
	Scan          int64
	After         string
	OriginalAfter string
	Through       string
	Partial       int64
}
type UnreadRow struct {
	ID     string
	Unread bool
}
