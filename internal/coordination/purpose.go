package coordination

import "context"

type purposeKey struct{}

func withCoordinationPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeKey{}, purpose)
}

func coordinationPurpose(ctx context.Context) string {
	purpose, ok := ctx.Value(purposeKey{}).(string)
	if !ok {
		return ""
	}
	return purpose
}
