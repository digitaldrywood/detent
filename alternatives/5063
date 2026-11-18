package store

import (
	"context"

	"github.com/digitaldrywood/detent/internal/policy"
)

type SessionPolicyStore interface {
	SessionPolicy(context.Context, string, int64) (policy.Descriptor, error)
}
