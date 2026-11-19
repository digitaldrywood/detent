package operatorchat

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type AuthorityDeps struct {
	Hosted            bool
	Identity          func(hubapi.APICredential, string) operatortool.Identity
	OriginCredential  func(context.Context, string) (hubapi.APICredential, error)
	TokenCredential   func(context.Context, string, string) (hubapi.APICredential, error)
	AccountCredential func(context.Context, string) (hubapi.APICredential, error)
	SessionCredential func(context.Context, string) (hubapi.APICredential, error)
	CurrentAuthority  func(context.Context, hubapi.APICredential, string) (operatortool.Authority, error)
}

func (s AuthorityDeps) Resolve(ctx context.Context, identity operatortool.Identity, origin string) (operatortool.Authority, error) {
	var credential hubapi.APICredential
	var err error
	if origin != "" {
		if identity.SessionID != "" {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		credential, err = s.OriginCredential(ctx, origin)
	} else if identity.SessionID == "" {
		credential, err = s.TokenCredential(ctx, identity.OrganizationID, identity.CredentialID)
	} else if s.Hosted && strings.HasPrefix(identity.PrincipalID, "account:") {
		credential, err = s.AccountCredential(ctx, identity.SessionID)
	} else if s.Hosted {
		credential, err = s.SessionCredential(ctx, identity.SessionID)
	} else {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	if err != nil || s.Identity(credential, identity.OrganizationID) != identity {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	return s.CurrentAuthority(ctx, credential, identity.OrganizationID)
}

type EntryOrigin struct {
	Issuer        string
	Generation    int64
	ResolveKey    entryauthority.Resolver
	ClassifyError func(error) error
}

func (s EntryOrigin) Claims(ctx context.Context, encoded string) (cloudassert.Claims, error) {
	var origin entryauthority.Origin
	if s.ResolveKey == nil || json.Unmarshal([]byte(encoded), &origin) != nil || origin.Issuer != s.Issuer || origin.Generation != s.Generation || len(origin.CredentialBinding) != 64 || origin.KeyID == "" || origin.Subject == "" {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	claims, err := s.ResolveKey(ctx, origin)
	if err != nil {
		return cloudassert.Claims{}, s.ClassifyError(err)
	}
	if claims.Key == nil || claims.Key.ID != origin.KeyID || claims.Key.CredentialBinding != origin.CredentialBinding || claims.Subject != origin.Subject || claims.Issuer != origin.Issuer || claims.Generation != origin.Generation {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	return claims, nil
}
