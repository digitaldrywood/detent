package searchstore

import (
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const TermLimit = 32
const MaxQueryTextBytes = 512

var ErrInvalid = errors.New("invalid search record or query")
var ErrIndexPending = errors.New("search index refresh is pending")
var ErrBusy = errors.New("search did not finish within its budget")

type Document struct {
	OrganizationID tracker.OrganizationID
	ID             string
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	Kind           string
	SourceID       string
	Scope          string
	Revision       int64
}

type Text struct {
	Title  string
	Body   string
	Labels []string
}

type Statistics struct {
	Tokens      int64
	Terms       []string
	Frequencies []int64
}

type Query struct {
	ProjectID tracker.ProjectID
	Kind      string
	Text      string
	Limit     int
}
