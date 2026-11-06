package dispatch

import (
	"context"
	"runtime"

	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) projectDiagnosticClaims(source *NativeConnector) []runnerauth.DiagnosticAttempt {
	return diagnostics.Claims(s.ActiveExecutions(), source, s.now(), s.leaseTTL)
}

func (s *Scheduler) enrichProjectDiagnostics(ctx context.Context, source *NativeConnector, view *runnerauth.ProjectConfiguration, update *runnerauth.UpdateObservation) error {
	supported, err := source.NativeClient().HubFeature(ctx, tracker.NativeProjectDiagnosticsCapability)
	if err != nil {
		return err
	}
	if !supported {
		view.Diagnostics = nil
		return nil
	}
	d := view.Diagnostics
	if d == nil {
		return nil
	}
	if update != nil {
		build := update.Running
		d.RunningBuild = &build
		delete(d.Unavailable, "running_build")
	}
	if d.RunningBuild == nil {
		s.mu.Lock()
		version := s.machine.Version
		s.mu.Unlock()
		if version != "" {
			d.RunningBuild = &runnerauth.BuildEvidence{Version: version, Commit: "unknown", Source: "unknown", OS: runtime.GOOS, Architecture: runtime.GOARCH, ObservedAt: s.now().UTC()}
			d.Unavailable["running_build"] = "unavailable"
		}
	}
	claims := s.projectDiagnosticClaims(source)
	d.Counts["native_claim"] = len(claims)
	for _, c := range claims {
		matched := false
		for i, r := range d.Records {
			if r.Key != c.Key {
				continue
			}
			c.Membership = append(r.Membership, c.Membership...)
			c.DeferredAt, c.RetryAt, c.RetryAttempt = r.DeferredAt, r.RetryAt, r.RetryAttempt
			if r.RecoveryOwner != "" {
				c.RecoveryOwner = r.RecoveryOwner
			}
			if c.ProviderCompletedAt.IsZero() {
				c.ProviderCompletedAt = r.ProviderCompletedAt
			}
			if c.HostOperation == "" {
				c.HostOperation, c.OperationPending, c.LatestError = r.HostOperation, r.OperationPending, r.LatestError
			}
			if !c.ProviderCompletedAt.IsZero() {
				delete(c.Unavailable, "provider_completion")
			}
			if c.HostOperation != "" {
				delete(c.Unavailable, "host_operation")
			}
			d.Records[i] = c
			matched = true
			break
		}
		if !matched {
			d.Records = append(d.Records, c)
		}
	}
	if d.Truncated {
		delete(d.Counts, "distinct_records")
		d.Unavailable["distinct_records"] = "unavailable"
	} else {
		d.Counts["distinct_records"] = len(d.Records)
	}
	d.Bound()
	return nil
}
