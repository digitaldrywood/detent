package changefiles

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository     tenantstore.Repository
	Details        func() attachment.DetailStore
	CurrentTime    func() (time.Time, error)
	Context        func(context.Context) context.Context
	ReadChange     func(context.Context, hubapi.Scope, string, string) (tracker.ChangeRequest, error)
	SourceAccess   func(context.Context, tracker.NativeWorkItemID) error
	ReadVersion    func(context.Context, tracker.OrganizationID, string, string) (eventstore.Version, error)
	Receipt        func(context.Context, hubapi.Scope, string, string, string) (json.RawMessage, bool, error)
	RecordError    func(error) error
	Scope          func(echo.Context) hubapi.Scope
	Error          func(echo.Context, error) error
	InvalidRequest func(echo.Context, error) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
