package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) sharedAccessSession(ctx context.Context, c echo.Context, claims cloudassert.Claims) (auth.Session, string, error) {
	session, hash, err := s.requestAuth().SharedAccessSession(ctx, c, claims)
	return session, hash, hubapi.FromFeatureError(err)
}

func (s *Service) entryAccessKeyCredential(ctx context.Context, claims cloudassert.Claims) (hubapi.APICredential, int, error) {
	return nativeCredentialResult(s.requestAuth().EntryKeyCredential(ctx, claims))
}

func (s *Service) ensureEntryServicePrincipal(ctx context.Context, claims cloudassert.Claims) error {
	return s.requestAuth().EnsureEntryServicePrincipal(ctx, claims)
}
