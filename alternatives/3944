package runnerstore

import "context"

type UpdateTarget struct {
	Version  string `json:"version"`
	Revision int64  `json:"revision"`
}

type UpdateTargetReader interface {
	RunnerUpdateTarget(context.Context, string) (UpdateTarget, error)
	RunnerVersionFailed(context.Context, string) (bool, error)
}

type UpdateTargetWriter interface {
	UpdateTargetReader
	SetRunnerUpdateTarget(context.Context, string, UpdateTarget) error
}
