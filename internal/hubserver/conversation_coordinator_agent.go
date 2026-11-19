package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

// withCoordinatorAgent attributes application writes made under ctx to the
// agent acting for the credential's principal.
func withCoordinatorAgent(ctx context.Context, agent string) context.Context {
	if resolve, ok := ctx.Value(hubOperatorResolverKey{}).(func(context.Context) (hubapi.APICredential, error)); ok {
		ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(ctx context.Context) (hubapi.APICredential, error) {
			credential, err := resolve(ctx)
			credential.Agent = agent
			return credential, err
		})
	}
	if resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error)); ok {
		ctx = context.WithValue(ctx, hubscope.NativeOperatorScopeKey{}, func(ctx context.Context) (hubscope.NativeScope, error) {
			scope, err := resolve(ctx)
			scope.Credential.Agent = agent
			return scope, err
		})
	}
	return ctx
}
