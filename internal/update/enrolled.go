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
	if ctx == nil {
		ctx = context.Background()
	}
	if request == nil {
		if s.operationMu.TryLock() {
			if ctx.Err() == nil {
				if _, pending := s.restartBehindControlLocked(); pending {
					s.operationMu.Unlock()
					return s.EnrolledObservation(running)
				}
			}
			lastCheck := s.Status().LastCheckAt
			s.mu.RLock()
			pendingVerification := !s.enrolledStateValid && s.enrolledReceipt != nil && s.enrolledReceipt.AdvancedFollowHubRelease()
			s.mu.RUnlock()
			refresh := pendingVerification && !s.cfg.Enabled && (lastCheck == nil || !s.cfg.Now().Before(lastCheck.Add(s.cfg.CheckInterval)))
			if s.cfg.WorkerArtifacts == nil && (s.enrolledDiscovery == "" || refresh) && s.cfg.Updater != nil {
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
			observed := s.EnrolledObservation(running)
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
		return s.EnrolledObservation(running)
	}
	if request.Urgent || request.FollowHub {
		s.operationMu.Lock()
	} else if !s.operationMu.TryLock() {
		return s.EnrolledObservation(running)
	}
	defer s.operationMu.Unlock()
	observed := s.EnrolledObservation(running)
	s.mu.RLock()
	stateValid := s.enrolledStateValid
	s.mu.RUnlock()
	if !stateValid || ctx.Err() != nil || request.Validate() != nil {
		return observed
	}
	sameRelease := request.Release && displayVersion(request.Version) == displayVersion(running.Version)
	if observed.Supported && sameRelease && observed.Running.VerifiedRelease {
		if receipt, ok := s.runningEnrolledReceipt(*request, observed.Running); ok {
			if receipt != nil {
				previous, _ := s.enrolledState()
				s.setEnrolledReceipt(receipt)
				if s.persistCurrentState() != nil {
					s.mu.Lock()
					s.enrolledReceipt = previous
					s.mu.Unlock()
					return nil
				}
				return s.EnrolledObservation(running)
			}
			return observed
		}
	}
	retry := observed.Receipt != nil && observed.Receipt.Request == *request && observed.Receipt.FailureReason != "" && observed.Receipt.FailureReason != enrolledTargetRefusal && (observed.Receipt.Status == "refused" || observed.Receipt.Status == "uncertain")
	if observed.Receipt != nil && observed.Receipt.Request.ID == request.ID {
		if observed.Supported && observed.Receipt.Request == *request && observed.Receipt.ExecutionReady() && sameRelease && running.SHA256 != "" && !observed.Running.VerifiedRelease && running.Source == "release" && s.cfg.ReserveIdle != nil {
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
	repair := sameRelease && running.SHA256 != "" && running.Source == "release" && !observed.Running.VerifiedRelease
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
	if !observed.Supported || request.Service != observed.Service || !targetMatches || sameRelease && running.SHA256 == "" || !request.Release && !observed.Pending {
		if installed != nil {
			observed.Receipt = receipt
			return observed
		}
		s.setEnrolledReceipt(receipt)
		if s.persistCurrentState() != nil {
			return nil
		}
		return s.EnrolledObservation(running)
	}
	return s.applyEnrolledRequestLocked(ctx, running, receipt, nil)
}

func (s *Scheduler) runningEnrolledReceipt(request runnerauth.UpdateRequest, running runnerauth.BuildEvidence) (*runnerauth.UpdateReceipt, bool) {
	receipt, _ := s.enrolledState()
	if receipt != nil {
		for _, target := range []*runnerauth.BuildEvidence{receipt.Applied, receipt.VerifiedTarget} {
			if target != nil && target.VerifiedRelease && !running.Matches(*target) {
				return nil, false
			}
		}
	}
	if receipt != nil && receipt.Request.ID == request.ID {
		if !receipt.Request.Matches(request) {
			return nil, false
		}
		if receipt.Status == "running" && receipt.Running != nil {
			return nil, true
		}
	} else if request.Urgent && receipt != nil && receipt.Applied != nil && receipt.Applied.VerifiedRelease && running.Matches(*receipt.Applied) {
		return nil, true
	} else {
		receipt = &runnerauth.UpdateReceipt{Request: request}
	}
	receipt.Status, receipt.FailureReason, receipt.ObservedAt = "running", "", running.ObservedAt
	receipt.Applied, receipt.Running = &running, &running
	return receipt, true
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
			if current := s.EnrolledObservation(running).Receipt; current != nil {
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
	return s.EnrolledObservation(running)
}

func (s *Scheduler) EnrolledObservation(running runnerauth.BuildEvidence) *runnerauth.UpdateObservation {
	s.mu.RLock()
	status := cloneAutoStatus(s.status)
	receipt := cloneEnrolledReceipt(s.enrolledReceipt)
	valid := s.enrolledStateValid
	discovery := s.enrolledDiscovery
	verifiedRunning := s.verifiedRunning
	s.mu.RUnlock()
	now := s.cfg.Now().UTC()
	running.ObservedAt = now
	if verifiedRunning && running.Matches(s.cfg.RunningBuild) {
		running.VerifiedRelease = true
		running.Source = "release"
	}
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
		Discovery: "unknown", Protocol: 1, Service: "detent", Supported: valid && s.cfg.Updater != nil && s.cfg.ReserveDrain != nil && s.cfg.RequestRestart != nil && (s.cfg.ReceiptStore != nil || strings.TrimSpace(s.cfg.StatePath) != ""),
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
	evidence := s.releaseBuildEvidence(applied)
	s.enrolledReceipt.Applied = &evidence
	if applied.ReplacementPending {
		s.enrolledReceipt.VerifiedTarget = s.enrolledReceipt.Applied
		s.enrolledReceipt.Applied = nil
		s.enrolledReceipt.Status = "uncertain"
	} else if evidence.VerifiedRelease && s.verifiedRunning && s.cfg.RunningBuild.Matches(evidence) {
		s.enrolledReceipt.Running = &evidence
		s.enrolledReceipt.Status = "running"
	}
}

func (s *Scheduler) releaseBuildEvidence(applied Status) runnerauth.BuildEvidence {
	return runnerauth.BuildEvidence{
		Version: applied.LatestVersion, Commit: enrolledCommit(applied.LatestCommit), Source: string(InstallSourceRelease), SHA256: applied.BinarySHA256,
		OS: s.cfg.RunningBuild.OS, Architecture: s.cfg.RunningBuild.Architecture, VerifiedRelease: applied.VerifiedRelease, ObservedAt: s.cfg.Now().UTC(),
	}
}

func (s *Scheduler) verifyRunningRelease(applied Status) bool {
	evidence := s.releaseBuildEvidence(applied)
	if !evidence.VerifiedRelease || evidence.Validate() != nil || applied.ReplacementPending || !s.cfg.RunningBuild.Matches(evidence) {
		return false
	}
	s.mu.Lock()
	s.verifiedRunning = true
	s.mu.Unlock()
	return true
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
