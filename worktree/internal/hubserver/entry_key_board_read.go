package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type entryBoardReadKey struct{}
type entryBoardRead struct{ credential apiCredential }

func entryBoardCredential(ctx context.Context) (apiCredential, bool) {
	read, ok := ctx.Value(entryBoardReadKey{}).(*entryBoardRead)
	if !ok {
		return apiCredential{}, false
	}
	return read.credential, true
}

func (s *Service) entryKeyReadAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	authorized := s.operatorAuthority(next)
	return func(c echo.Context) error {
		repository, supported := s.config.TenantRepository.(tenantstore.RequestReader)
		claims, shared := hostedSharedClaims(c)
		if !supported || !shared || claims.Kind != cloudassert.KindKey || claims.Key == nil {
			return authorized(c)
		}
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return s.invalidAPIRequest(c, err)
		}
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &call) != nil || call.Name != operatortool.WorkList {
			return authorized(c)
		}
		if len(claims.Key.CredentialBinding) == 64 && s.config.Hosted.SharedEntry.ResolveKey != nil {
			encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
			if err != nil {
				return err
			}
			claims, err = s.currentEntryOriginClaims(c.Request().Context(), string(encoded))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			c.Set(hostedSharedClaimsKey, claims)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		c.SetRequest(c.Request().WithContext(ctx))
		read := func() error {
			return repository.ReadRequest(c.Request().Context(), s.accessOrganization(), func(ctx context.Context) error {
				ctx = context.WithValue(ctx, entryBoardReadKey{}, &entryBoardRead{})
				credential, _, err := s.entryKeyCredential(ctx, claims)
				if err != nil {
					return err
				}
				c.SetRequest(c.Request().WithContext(context.WithValue(ctx, entryBoardReadKey{}, &entryBoardRead{credential: credential})))
				return authorized(c)
			})
		}
		err = read()
		if errors.Is(err, corestore.ErrNotFound) && claims.Key.Kind == "service" {
			if err := s.ensureEntryServicePrincipal(c.Request().Context(), claims); err != nil {
				return s.nativeAPIError(c, operatorAuthorizationError(err))
			}
			err = read()
		}
		if err != nil && !c.Response().Committed {
			return s.nativeAPIError(c, operatorAuthorizationError(err))
		}
		return err
	}
}
