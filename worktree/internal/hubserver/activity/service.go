package activity

import (
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

var ErrUnavailable = errors.New("operator command is unavailable")

type Deps struct {
	Repository          tenantstore.Repository
	Details             func() attachment.DetailStore
	Now                 func() time.Time
	Organization        func() string
	MissingParticipants error
	Unavailable         error
}
type Caller struct {
	Credential hubauth.Credential
	Scope      hubapi.Scope
	IsNotFound func(error) bool
}
type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.Unavailable == nil {
		deps.Unavailable = ErrUnavailable
	}
	return &Service{deps: deps}
}
