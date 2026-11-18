package threads

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
)

type HTTPDeps struct {
	Service      func() *Service
	Auth         func() *hubauth.Authenticator
	Operator     echo.MiddlewareFunc
	Error        func(echo.Context, error) error
	Resolve      func(context.Context) (hubauth.Input, error)
	Caller       func(hubauth.Input) Caller
	NotifyUnread func()
}

type HTTPService struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTPService {
	if deps.Resolve == nil {
		deps.Resolve = hubauth.Resolve
	}
	return &HTTPService{deps: deps}
}
func (s *HTTPService) caller(input hubauth.Input) Caller {
	if s.deps.Caller != nil {
		return s.deps.Caller(input)
	}
	return NewCaller(s.deps.Auth(), input, CallerDeps{})
}
