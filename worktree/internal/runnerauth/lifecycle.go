package runnerauth

import (
	"errors"
	"time"
)

type LifecycleState string

const (
	LifecycleStarting     LifecycleState = "starting"
	LifecycleReady        LifecycleState = "ready"
	LifecycleDraining     LifecycleState = "draining"
	LifecycleUpdating     LifecycleState = "updating"
	LifecycleShuttingDown LifecycleState = "shutting_down"
	LifecycleStopped      LifecycleState = "stopped"
	LifecycleStartFailed  LifecycleState = "start_failed"
)

type LifecycleReport struct {
	ProcessID     string         `json:"process_id"`
	Sequence      int64          `json:"sequence"`
	State         LifecycleState `json:"state"`
	Version       string         `json:"version"`
	BuildRevision string         `json:"build_revision"`
	TargetVersion string         `json:"target_version,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	ErrorCode     string         `json:"error_code,omitempty"`
}

type Lifecycle struct {
	LifecycleReport
	LastTransitionAt        time.Time `json:"last_transition_at"`
	ConsecutiveFailedStarts int       `json:"consecutive_failed_starts"`
}

func (r LifecycleReport) Validate() error {
	if r.ProcessID == "" || len(r.ProcessID) > 128 || r.Sequence < 1 || r.Version == "" || len(r.Version) > 128 || r.BuildRevision == "" || len(r.BuildRevision) > 128 || len(r.TargetVersion) > 128 {
		return errors.New("invalid runner lifecycle identity")
	}
	if r.State != LifecycleShuttingDown && r.Reason != "" || r.State != LifecycleStartFailed && r.ErrorCode != "" {
		return errors.New("invalid lifecycle transition detail")
	}
	switch r.State {
	case LifecycleStarting, LifecycleReady, LifecycleDraining, LifecycleStopped:
	case LifecycleUpdating:
		if r.TargetVersion == "" {
			return errors.New("update target required")
		}
	case LifecycleShuttingDown:
		switch r.Reason {
		case "update", "operator_stop", "drain_complete", "signal":
		default:
			return errors.New("shutdown reason required")
		}
	case LifecycleStartFailed:
		switch r.ErrorCode {
		case "startup_error", "ready_timeout", "repeated_start":
		default:
			return errors.New("startup error code required")
		}
	default:
		return errors.New("invalid runner lifecycle state")
	}
	return nil
}

const RunnerReadyTimeout = 2 * time.Minute

func (l Lifecycle) Effective(now time.Time) Lifecycle {
	if l.State == LifecycleStarting && !now.Before(l.LastTransitionAt.Add(2*time.Minute)) {
		l.State = LifecycleStartFailed
		l.ErrorCode = "ready_timeout"
		l.LastTransitionAt = l.LastTransitionAt.Add(RunnerReadyTimeout)
	}
	return l
}

func (r *Runner) ApplyLifecycle(now time.Time) {
	if r.Lifecycle == nil {
		return
	}
	current := r.Lifecycle.Effective(now)
	r.Lifecycle = &current
	if r.Health == "revoked" || r.Health == "expired" {
		return
	}
	switch current.State {
	case LifecycleStartFailed:
		r.Health = "needs_attention"
		r.ConnectionHealth = "offline"
	case LifecycleStarting, LifecycleShuttingDown, LifecycleStopped:
		r.Health = "offline"
		r.ConnectionHealth = "offline"
	}
}
