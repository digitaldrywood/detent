package viewstore

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrInvalidView = errors.New("saved view is invalid")

var ErrOwnerLimit = errors.New("saved view owner limit reached")
var ErrOrganizationLimit = errors.New("saved view organization limit reached")

type State struct {
	View            string
	Tab             string
	Q               string
	Sort            string
	CompletedWindow string
	CompletedFrom   string
	CompletedTo     string
	Archived        bool
	CreatedBy       string
	State           []string
	Type            []string
	Label           []string
	Assignee        []string
	Priority        []string
	Lanes           []string
	Collapsed       []string
}

type Record struct {
	OrganizationID tracker.OrganizationID
	ID             string
	OwnerID        string
	Name           string
	Scope          string
	ProjectID      *tracker.ProjectID
	ProjectIDs     []tracker.ProjectID
	Favorite       bool
	State          State
}

type Selection struct {
	UserID               string
	EnforceProjectGrants bool
}

type Reader interface {
	BoardReader
	WorkViewPreference(context.Context, string, string) (string, error)
	SavedWorkView(context.Context, Selection, string) (Record, error)
	SavedWorkViewIDs(context.Context, Selection, string, int) ([]string, error)
}

type Writer interface {
	BoardWriter
	Reader
	SetWorkViewPreference(context.Context, string, string, *string) error
	PutSavedWorkView(context.Context, Selection, Record, bool) error
	DeleteSavedWorkView(context.Context, Selection, string) error
	SetSavedWorkViewFavorite(context.Context, Selection, string, bool) error
}
