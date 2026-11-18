package runnerstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type LifecycleReader interface {
	RunnerLifecycle(context.Context, string) (*runnerauth.Lifecycle, error)
	RunnerLifecycleHistory(context.Context, string, int) ([]runnerauth.Lifecycle, error)
}

type LifecycleMaintainer interface {
	RunnerStartDeadlines(context.Context, time.Time, int) ([]string, error)
	ExpireRunnerStart(context.Context, string, time.Time) error
}

type LifecycleWriter interface {
	LifecycleReader
	ReportRunnerLifecycle(context.Context, string, runnerauth.LifecycleReport, time.Time) error
}
