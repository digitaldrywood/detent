package searchstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const CandidateLimit = 2000
const TermLimit = 32
const LexemeLimit = 128
const MaxQueryTextBytes = 512

var ErrInvalid = errors.New("invalid search record or query")
var ErrIndexPending = errors.New("search index refresh is pending")

type Refresh func(context.Context) (bool, error)

type Source struct {
	Vector     string
	Statistics Statistics
}

type Document struct {
	Generation     int64 `json:"-"`
	Placeholder    bool  `json:"-"`
	OrganizationID tracker.OrganizationID
	ID             string
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	Kind           string
	SourceID       string
	Revision       int64
	Title          string
	Scope          string
	Status         string
	UpdatedAt      time.Time
	Source         *Source `json:"-"`
	Payload        attachment.ArchiveReference
}

type Text struct {
	Title  string
	Body   string
	Labels []string
}

type Progress struct {
	AfterID string
	Payload attachment.ArchiveReference
}

type Prepared struct {
	Payload    attachment.ArchiveReference
	Document   Document
	Vector     string
	Literals   []LiteralPosting
	Statistics Statistics
}

type LiteralPosting struct {
	Term      string
	Positions []int64
}

type Statistics struct {
	Tokens      int64
	Terms       []string
	Frequencies []int64
}

type Query struct {
	ProjectID       tracker.ProjectID
	ProjectIDs      []tracker.ProjectID
	WorkItemID      tracker.NativeWorkItemID
	Kind            string
	Scope           string
	Status          string
	Text            string
	Archived        string
	Reporter        *string
	Exact           bool
	Limit           int
	Cursor          string
	BeforeUpdatedAt *time.Time
	BeforeID        string
	Streams         []StreamPosition
}

type StreamPosition struct {
	Lexeme    string `json:",omitempty"`
	Key       string
	ID        string
	Impact    float64
	UpdatedAt time.Time
	Done      bool
}

type Hit struct {
	Stream       *StreamPosition
	ScanBoundary bool
	Document     Document
	Coverage     int64
	Score        float64
	Number       int64
	ProjectName  string
	Snippet      string
	Index        *Statistics
}

type Page struct {
	Items      []Hit
	NextCursor string
}

type Reader interface {
	corestore.Reader
	Search(context.Context, Query) ([]Hit, error)
	PendingSearch(context.Context) (Document, error)
	SearchProgress(context.Context, Document) (Progress, error)
	SearchSources(context.Context, Document, string, int) ([]Document, error)
}

type Writer interface {
	corestore.Writer
	Reader
	AdvanceSearch(context.Context, Document) error
	SaveSearchProgress(context.Context, Document, Progress) (bool, error)
	PutSearch(context.Context, Document, int64) error
	DeleteSearch(context.Context, string, int64) error
	PublishSearch(context.Context, Prepared) (bool, error)
}

type Repository interface {
	ReadSearch(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteSearch(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}
