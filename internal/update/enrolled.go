package update

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (s *Scheduler) EnrolledUpdate(ctx context.Context, running runnerauth.BuildEvidence, request *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
	if s == nil {
		return nil
	}
	if request == nil {
		if s.operationMu.TryLock() {
			if s.enrolledDiscovery == "" && s.cfg.Updater != nil {
				s.updateStatus(func(*AutoStatus) { s.enrolledDiscovery = "unknown" })
				if !s.cfg.Enabled {
					checked, err := s.cfg.Updater.Check(ctx)
					if err == nil {
						s.updateStatus(func(status *AutoStatus) {
							checkedAt := s.cfg.Now().UTC()
							status.LastCheckAt = &checkedAt
							status.LastError = ""
							if checked.UpdateAvailable {
								s.enrolledDiscovery = "available"
								status.AvailableVersion = checked.LatestVersion
							} else {
								s.enrolledDiscovery = "up_to_date"
								status.AvailableVersion = ""
							}
						})
					} else {
						s.updateStatus(func(status *AutoStatus) { status.LastError = "Configured release discovery is unavailable" })
					}
				}
			}
			observed := s.enrolledObservation(running)
			if observed.Receipt != nil && observed.Receipt.Status == "running" {
				s.mu.RLock()
				needsReceipt := s.enrolledReceipt != nil && s.enrolledReceipt.Running == nil
				s.mu.RUnlock()
				if needsReceipt {
					s.setEnrolledReceipt(observed.Receipt)
					if s.persistCurrentState() != nil {
						s.operationMu.Unlock()
						return nil
					}
				}
			}
			s.operationMu.Unlock()
		}
		return s.enrolledObservation(running)
	}
	if request.Urgent || request.FollowHub {
		s.operationMu.Lock()
	} else if !s.operationMu.TryLock() {
		return s.enrolledObservation(running)
	}
	defer s.operationMu.Unlock()
	observed := s.enrolledObservation(running)
	retry := observed.Receipt != nil && observed.Receipt.Request == *request && observed.Receipt.FailureReason != "" && (observed.Receipt.Status == "refused" || observed.Receipt.Status == "uncertain")
	if observed.Receipt != nil && observed.Receipt.Request.ID == request.ID {
		if !retry || s.cfg.Now().Before(observed.Receipt.ObservedAt.Add(s.cfg.CheckInterval)) {
			return observed
		}
	}
	receipt := &runnerauth.UpdateReceipt{Request: *request, Status: "refused", ObservedAt: s.cfg.Now().UTC()}
	targetMatches := retry || request.ExpectedBuildRevision == observed.Revision && request.Version == observed.AvailableVersion
	if request.FollowHub {
		targetMatches = retry || request.ExpectedBuildRevision == observed.Revision && !IsDevelopmentVersion(request.Version) && strings.TrimPrefix(request.Version, "v") != strings.TrimPrefix(running.Version, "v")
	}
	if request.Urgent {
		comparison, err := CompareVersions(request.Version, running.Version)
		targetMatches = err == nil && comparison > 0
		if applied := s.Status().LastAppliedVersion; applied != "" && (!retry || applied != request.Version) {
			comparison, err = CompareVersions(request.Version, applied)
			targetMatches = targetMatches && err == nil && comparison > 0
		}
	}
	if request.Validate() != nil || !observed.Supported || request.Service != observed.Service || !targetMatches || !request.Release && !observed.Pending {
		if request.Validate() != nil {
			return observed
		}
		s.setEnrolledReceipt(receipt)
		if s.persistCurrentState() != nil {
			return nil
		}
		return s.enrolledObservation(running)
	}
	receipt.Status = "draining"
	s.setEnrolledReceipt(receipt)
	if s.persistCurrentState() != nil {
		return nil
	}
	opts := s.cfg.ApplyOptions
	opts.ExpectedVersion = request.Version
	opts.FromRelease = request.FromRelease
	opts.Urgent = request.Urgent
	opts.FollowHub = request.FollowHub
	applied, err := s.drainAndApplyWithOptionsLocked(ctx, opts)
	if err != nil || applied.Action != ActionUpdated || applied.FailureReason != "" {
		receipt.Status = "refused"
		if applied.Action == ActionUpdated {
			if current := s.enrolledObservation(running).Receipt; current != nil {
				receipt = current
			}
			receipt.Status = "uncertain"
		}
		receipt.FailureReason = applied.FailureReason
		if receipt.FailureReason == "" {
			receipt.FailureReason = "Update apply failed or was refused"
		}
		receipt.ObservedAt = s.cfg.Now().UTC()
		s.setEnrolledReceipt(receipt)
		if s.persistCurrentState() != nil {
			return nil
		}
	}
	return s.enrolledObservation(running)
}

func (s *Scheduler) enrolledObservation(running runnerauth.BuildEvidence) *runnerauth.UpdateObservation {
	status := s.Status()
	s.mu.RLock()
	receipt := cloneEnrolledReceipt(s.enrolledReceipt)
	valid := s.enrolledStateValid
	discovery := s.enrolledDiscovery
	s.mu.RUnlock()
	now := s.cfg.Now().UTC()
	running.ObservedAt = now
	if receipt != nil {
		if receipt.Applied == nil && receipt.VerifiedTarget != nil && running.Matches(*receipt.VerifiedTarget) {
			applied := *receipt.VerifiedTarget
			applied.ObservedAt = now
			receipt.Applied = &applied
		}
		if receipt.Applied != nil && running.Matches(*receipt.Applied) {
			receipt.Status = "running"
			receipt.FailureReason = ""
			running.VerifiedRelease = receipt.Applied.VerifiedRelease
			if running.VerifiedRelease {
				running.Source = "release"
			}
			if receipt.Running == nil {
				witnessed := running
				receipt.Running = &witnessed
			}
		} else if receipt.Running != nil {
			receipt.Status = "applied"
		} else if receipt.Applied != nil && status.State == "restart_requested" {
			receipt.Status = "restart_requested"
		} else if receipt.Status == "running" || receipt.Status == "draining" && status.State != "draining" && status.State != "applying" {
			receipt.Status = "uncertain"
		}
	}
	observation := &runnerauth.UpdateObservation{
		Discovery: "unknown", Protocol: 1, Service: "detent", Supported: valid && s.cfg.Updater != nil && s.cfg.ReserveDrain != nil && s.cfg.RequestRestart != nil && strings.TrimSpace(s.cfg.StatePath) != "",
		Pending: status.State == "pending_idle", AvailableVersion: status.AvailableVersion,
		Running: running, Receipt: receipt, ObservedAt: now,
	}
	if status.LastError == "" {
		if status.AvailableVersion != "" {
			observation.Discovery = "available"
		} else if status.State == "up_to_date" || discovery == "up_to_date" {
			observation.Discovery = "up_to_date"
		}
	}
	if status.LastCheckAt != nil {
		observation.AvailableObservedAt = *status.LastCheckAt
	}
	observation.Revision = observation.BuildRevision()
	return observation
}

func (s *Scheduler) setEnrolledReceipt(receipt *runnerauth.UpdateReceipt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrolledReceipt = cloneEnrolledReceipt(receipt)
}

func cloneEnrolledReceipt(receipt *runnerauth.UpdateReceipt) *runnerauth.UpdateReceipt {
	if receipt == nil {
		return nil
	}
	copy := *receipt
	if receipt.Running != nil {
		running := *receipt.Running
		copy.Running = &running
	}
	if receipt.VerifiedTarget != nil {
		target := *receipt.VerifiedTarget
		copy.VerifiedTarget = &target
	}
	if receipt.Applied != nil {
		applied := *receipt.Applied
		copy.Applied = &applied
	}
	return &copy
}

func (s *Scheduler) recordEnrolledApplied(applied Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrolledReceipt == nil || s.enrolledReceipt.Status != "draining" {
		return
	}
	s.enrolledReceipt.Status = "applied"
	s.enrolledReceipt.ObservedAt = s.cfg.Now().UTC()
	s.enrolledReceipt.Applied = &runnerauth.BuildEvidence{
		Version: applied.LatestVersion, Commit: enrolledCommit(applied.LatestCommit), Source: string(InstallSourceRelease), SHA256: applied.BinarySHA256,
		OS: s.cfg.RunningBuild.OS, Architecture: s.cfg.RunningBuild.Architecture, VerifiedRelease: applied.VerifiedRelease, ObservedAt: s.cfg.Now().UTC(),
	}
	if applied.ReplacementPending {
		s.enrolledReceipt.VerifiedTarget = s.enrolledReceipt.Applied
		s.enrolledReceipt.Applied = nil
		s.enrolledReceipt.Status = "uncertain"
	}
}

func (s *Scheduler) enrolledState() (*runnerauth.UpdateReceipt, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	receipt := cloneEnrolledReceipt(s.enrolledReceipt)
	if receipt == nil {
		return nil, time.Time{}
	}
	return receipt, receipt.ObservedAt
}

func enrolledCommit(commit string) string {
	if commit == "" {
		return "none"
	}
	return commit
}

func (s *Scheduler) RunningBuild() runnerauth.BuildEvidence { return s.cfg.RunningBuild }
