package runnerstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type LifecycleRefusal string

const (
	LifecycleInvalidTransition   LifecycleRefusal = "invalid_transition"
	LifecycleRunningBuildChanged LifecycleRefusal = "running_build_changed"
	LifecycleUpdateNotOffered    LifecycleRefusal = "update_not_offered"
)

type LifecycleTransition struct {
	runnerauth.Lifecycle
	Refusal          LifecycleRefusal
	RolloutRequestID string
}

type Rollout struct {
	RequestID            string
	Version              string
	OrganizationRevision int64
}

type LifecycleReader interface {
	RunnerLifecycle(context.Context, string) (*runnerauth.Lifecycle, error)
	RunnerLifecycleHistory(context.Context, string, int) ([]LifecycleTransition, error)
}

type LifecycleMaintainer interface {
	RunnerStartDeadlines(context.Context, time.Time, int) ([]string, error)
	ExpireRunnerStart(context.Context, string, time.Time) error
}

type LifecycleWriter interface {
	LifecycleReader
	UpdateLocker
	RunnerLifecycleReceipt(context.Context, string, runnerauth.LifecycleReport, string) (LifecycleRefusal, bool, error)
	RefuseRunnerLifecycle(context.Context, string, runnerauth.LifecycleReport, string, LifecycleRefusal, time.Time) error
	ReportRunnerLifecycle(context.Context, string, runnerauth.LifecycleReport, string, Rollout, time.Time) error
}
