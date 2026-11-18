package usagecosts

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Window struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Hourly bool      `json:"-"`
}

type Deps struct {
	Repository           tenantstore.Repository
	Details              attachment.DetailStore
	Now                  func() time.Time
	Prices               Prices
	ContentContext       func(context.Context) context.Context
	RecordError          func(error) error
	RequireCollaboration func(context.Context, tenantstore.Writer, time.Time) error
	AccountUsage         func(context.Context, tenantstore.Writer, time.Time, string) error
	ValidateDetail       func(context.Context, attachment.DetailStore, tracker.OrganizationID, attachment.ArchiveReference, []byte) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service {
	return &Service{deps: deps}
}
