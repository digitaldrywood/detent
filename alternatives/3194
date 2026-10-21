package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const enrolledTargetRefusal = "Requested update target is not applicable to the observed runner state"

func (s *Scheduler) EnrolledUpdate(ctx context.Context, running runnerauth.BuildEvidence, request *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
	if s == nil {
		return nil
	}
	if request == nil {
		if s.operationMu.TryLock() {
			lastCheck := s.Status().LastCheckAt
			s.mu.RLock()
			pendingVerification := !s.enrolledStateValid && s.enrolledReceipt != nil && s.enrolledReceipt.AdvancedFollowHubRelease()
			s.mu.RUnlock()
			refresh := (s.cfg.WorkerArtifacts != nil || pendingVerification) && !s.cfg.Enabled && (lastCheck == nil || !s.cfg.Now().Before(lastCheck.Add(s.cfg.CheckInterval)))
			if (s.enrolledDiscovery == "" || refresh) && s.cfg.Updater != nil {
				s.updateStatus(func(*AutoStatus) { s.enrolledDiscovery = "unknown" })
				if !s.cfg.Enabled {
					checkedAt := s.cfg.Now().UTC()
					s.updateStatus(func(status *AutoStatus) { status.LastCheckAt = &checkedAt })
					checked, err := s.checkLocked(ctx)
					if err == nil {
						s.updateStatus(func(status *AutoStatus) {
							status.LastError = ""
							if checked.UpdateAvailable || checked.RepairAvailable {
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
			s.restartBehindControlLocked(ctx)
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
	s.mu.RLock()
	stateValid := s.enrolledStateValid
	s.mu.RUnlock()
	if !stateValid {
		return observed
	}
	retry := observed.Receipt != nil && observed.Receipt.Request == *request && observed.Receipt.FailureReason != "" && observed.Receipt.FailureReason != enrolledTargetRefusal && (observed.Receipt.Status == "refused" || observed.Receipt.Status == "uncertain")
	if observed.Receipt != nil && observed.Receipt.Request.ID == request.ID {
		if observed.Supported && observed.Receipt.Request == *request && observed.Receipt.ExecutionReady() && !running.VerifiedRelease && running.Source == "release" && strings.TrimPrefix(request.Version, "v") == strings.TrimPrefix(running.Version, "v") && s.cfg.ReserveIdle != nil {
			releaseIdle, idle := s.cfg.ReserveIdle(ctx)
			if idle {
				return s.applyEnrolledRequestLocked(ctx, running, observed.Receipt, releaseIdle)
			}
			if releaseIdle != nil {
				releaseIdle()
			}
			return observed
		}
		if !retry || s.cfg.Now().Before(observed.Receipt.ObservedAt.Add(s.cfg.CheckInterval)) {
			return observed
		}
	}
	if request.Validate() != nil {
		return observed
	}
	var installed *runnerauth.BuildEvidence
	if observed.Receipt != nil {
		installed = observed.Receipt.Applied
		if installed == nil {
			installed = observed.Receipt.VerifiedTarget
		}
	}
	state := s.Status().State
	installedCurrent := observed.Running.VerifiedRelease || state == "restart_requested" || state == "applied" || state == "applied_restart_deferred"
	if !retry && observed.Supported && request.Release && installedCurrent && installed != nil && installed.VerifiedRelease && request.Version == installed.Version {
		return observed
	}
	receipt := &runnerauth.UpdateReceipt{Request: *request, Status: "refused", FailureReason: enrolledTargetRefusal, ObservedAt: s.cfg.Now().UTC()}
	repair := request.Release && strings.TrimPrefix(request.Version, "v") == strings.TrimPrefix(running.Version, "v") && running.Source == "release" && !observed.Running.VerifiedRelease
	targetMatches := retry || request.ExpectedBuildRevision == observed.Revision && (request.Version == observed.AvailableVersion || repair)
	if request.FollowHub {
		comparison, err := CompareVersions(request.Version, running.Version)
		targetMatches = err == nil && (comparison > 0 || comparison == 0 && repair) && (retry || request.ExpectedBuildRevision == observed.Revision)
	}
	if request.Urgent {
		comparison, err := CompareVersions(request.Version, running.Version)
		targetMatches = err == nil && comparison > 0
		if applied := s.Status().LastAppliedVersion; applied != "" && (!retry || applied != request.Version) {
			comparison, err = CompareVersions(request.Version, applied)
			targetMatches = targetMatches && err == nil && comparison > 0
		}
	}
	if repair {
		if applied := s.Status().LastAppliedVersion; applied != "" {
			comparison, err := CompareVersions(request.Version, applied)
			targetMatches = targetMatches && err == nil && comparison >= 0
		}
	}
	if !observed.Supported || request.Service != observed.Service || !targetMatches || !request.Release && !observed.Pending {
		if installed != nil {
			observed.Receipt = receipt
			return observed
		}
		s.setEnrolledReceipt(receipt)
		if s.persistCurrentState() != nil {
			return nil
		}
		return s.enrolledObservation(running)
	}
	return s.applyEnrolledRequestLocked(ctx, running, receipt, nil)
}

func (s *Scheduler) applyEnrolledRequestLocked(ctx context.Context, running runnerauth.BuildEvidence, receipt *runnerauth.UpdateReceipt, releaseIdle func()) *runnerauth.UpdateObservation {
	request := &receipt.Request
	receipt.Status = "applying"
	receipt.FailureReason = ""
	s.setEnrolledReceipt(receipt)
	if s.persistCurrentState() != nil {
		if releaseIdle != nil {
			releaseIdle()
		}
		return nil
	}
	opts := s.cfg.ApplyOptions
	opts.ExpectedVersion = request.Version
	opts.FromRelease = request.FromRelease
	opts.Urgent = request.Urgent
	opts.FollowHub = request.FollowHub
	var applied Status
	var err error
	if releaseIdle != nil {
		applied, err = s.applyWithOptionsLocked(ctx, releaseIdle, opts)
	} else {
		applied, err = s.applyRequestedLocked(ctx, opts)
	}
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
		attrs := []any{"status", receipt.Status, "request_id", request.ID, "version", request.Version, "failure_reason", receipt.FailureReason}
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		s.cfg.Logger.Warn("enrolled runner update not applied", attrs...)
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
	if !valid {
		receipt = nil
	}
	if receipt != nil && receipt.AdvancedFollowHubRelease() && running.Matches(*receipt.Applied) {
		running.VerifiedRelease = true
		running.Source = "release"
	}
	if receipt != nil {
		if receipt.Applied == nil && receipt.VerifiedTarget != nil && running.VerifiedRelease && running.Matches(*receipt.VerifiedTarget) {
			applied := *receipt.VerifiedTarget
			applied.ObservedAt = now
			receipt.Applied = &applied
		}
		if receipt.Applied != nil && running.VerifiedRelease && running.Matches(*receipt.Applied) {
			receipt.Status = "running"
			receipt.FailureReason = ""
			if receipt.Running == nil {
				witnessed := running
				receipt.Running = &witnessed
			}
		} else if receipt.Running != nil && receipt.FailureReason == "" {
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
	copy := cloneEnrolledReceipt(receipt)
	copy.RetainPreviousFailure(s.enrolledReceipt)
	s.enrolledReceipt = copy
}

func cloneEnrolledReceipt(receipt *runnerauth.UpdateReceipt) *runnerauth.UpdateReceipt {
	if receipt == nil {
		return nil
	}
	copy := *receipt
	if receipt.PreviousFailure != nil {
		previous := *receipt.PreviousFailure
		copy.PreviousFailure = &previous
	}
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

func (s *Scheduler) recordEnrolledApplied(applied Status, executionArtifact bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrolledReceipt == nil || s.enrolledReceipt.Status != "draining" && s.enrolledReceipt.Status != "applying" {
		return
	}
	s.enrolledReceipt.ExecutionArtifact = executionArtifact
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

func (s *Scheduler) enrolledApplyOptions(opts ApplyOptions) ApplyOptions {
	receipt, _ := s.enrolledState()
	if receipt != nil && (receipt.Status == "applying" || receipt.Status == "draining") {
		opts.ExpectedVersion = receipt.Request.Version
		opts.FromRelease = receipt.Request.FromRelease
		opts.FollowHub = receipt.Request.FollowHub
		opts.Urgent = receipt.Request.Urgent
	}
	return opts
}

func verifyAdvancedEnrolledReceipt(ctx context.Context, cfg SchedulerConfig, receipt *runnerauth.UpdateReceipt) error {
	if receipt == nil || !receipt.AdvancedFollowHubRelease() {
		return nil
	}
	service, ok := cfg.Updater.(*Service)
	if !ok || cfg.RunningBuild.Validate() != nil || !cfg.RunningBuild.Matches(*receipt.Applied) || receipt.Applied.OS != service.cfg.GOOS || receipt.Applied.Architecture != service.cfg.GOARCH {
		return errors.New("advanced follow-Hub receipt requires matching signed release evidence")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPClientTimeout)
	defer cancel()
	release, _, err := service.publishedRelease(ctx, "v"+displayVersion(receipt.Applied.Version))
	if err != nil {
		return err
	}
	status, prepared, err := service.prepareReleaseBinary(ctx, Status{LatestVersion: displayVersion(release.TagName), LatestTag: release.TagName}, release)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "detent-update-receipt-*")
	if err != nil {
		return err
	}
	defer removeWorkerStaging(staging)
	path := filepath.Join(staging, projectName)
	identity := binaryIdentityVerifier(service.cfg.BinaryVerifier, status.LatestVersion, status.LatestCommit)
	if err := replaceBinaryNow(ctx, Replacement{Context: ctx, Target: path, Binary: prepared.binary, Mode: prepared.mode, GOOS: service.cfg.GOOS, Verify: identity, Sign: service.cfg.BinarySigner}); err != nil {
		return err
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(installed)
	if status.LatestCommit != receipt.Applied.Commit || hex.EncodeToString(sum[:]) != receipt.Applied.SHA256 {
		return errors.New("advanced follow-Hub receipt does not match its signed release")
	}
	return nil
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
