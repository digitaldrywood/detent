package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/auth"
)

func (*hostedLoginProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}

func (*hostedLoginProvider) RefreshAccess(context.Context, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}

func (*browserHostedProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}

func (*browserHostedProvider) RefreshAccess(context.Context, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}

func (*hostedSecurityProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}

func (*hostedSecurityProvider) RefreshAccess(context.Context, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}
