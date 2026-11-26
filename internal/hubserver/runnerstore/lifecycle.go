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
	ActivationID         string `json:"activation_id"`
	RequestID            string `json:"update_request_id"`
	Version              string `json:"version"`
	OrganizationRevision int64  `json:"-"`
}

type RestartEvidence struct {
	ActivationID string                    `json:"activation_id"`
	Version      string                    `json:"version"`
	Outcome      runnerauth.LifecycleState `json:"outcome"`
	FailedStarts int                       `json:"consecutive_failed_starts"`
}

type RestartEvidenceReader interface {
	RunnerRollout(context.Context, string) (Rollout, error)
}

type RestartEvidenceWriter interface {
	RecordRunnerRestartEvidence(context.Context, string, string, RestartEvidence, time.Time) error
}

type LifecycleStartConflict struct {
	Generation int64
}

func (LifecycleStartConflict) Error() string { return "runner process generation changed" }

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
	ReportRunnerLifecycle(context.Context, string, runnerauth.LifecycleReport, int64, string, Rollout, time.Time) error
}
