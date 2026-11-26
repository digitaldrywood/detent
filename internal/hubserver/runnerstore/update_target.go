package runnerstore

import "context"

type UpdateTarget struct {
	Version  string `json:"version"`
	Revision int64  `json:"revision"`
}

type UpdateTargetReader interface {
	RunnerUpdateTarget(context.Context, string) (UpdateTarget, error)
	RunnerVersionFailed(context.Context, string, string) (bool, error)
}

type UpdateTargetWriter interface {
	UpdateTargetReader
	LockOrganizationUpdateTarget(context.Context, bool) error
	SetRunnerUpdateTarget(context.Context, string, UpdateTarget) error
}
