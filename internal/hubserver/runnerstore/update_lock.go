package runnerstore

import "context"

type UpdateLocker interface {
	LockRunnerUpdate(context.Context, string) error
}
