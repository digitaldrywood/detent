package billingstore

import "context"

type RuntimeRevision struct {
	ID        string
	Published bool
	Reference AuditReference
}

type RuntimeReader interface {
	CurrentRuntime(context.Context) (RuntimeRevision, error)
	RuntimePublished(context.Context, string) (bool, error)
}

type RuntimeWriter interface {
	RuntimeReader
	StageRuntime(context.Context, RuntimeRevision) error
}
