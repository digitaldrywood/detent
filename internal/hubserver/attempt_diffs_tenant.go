package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) postAttemptDiffTenant(ctx context.Context, scope nativeScope, attemptID string, request tracker.AttemptDiffRequest, checkOnly bool) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	if s.config.TenantRepository == nil || s.runDetailStore() == nil {
		return receipt, nativeRunEventStorageUnavailable()
	}
	if request.Producer.Kind != tracker.DiffSourceAttempt {
		return receipt, nativeStaleExecution("Only the attempt's own lease may write this diff until a workspace session owns it")
	}
	if request.Producer.ID != "" && request.Producer.ID != attemptID {
		return receipt, nativeStaleExecution("An attempt producer may only write its own attempt's diff")
	}
	if strings.TrimSpace(string(request.Producer.LeaseID)) == "" || request.Producer.FencingToken <= 0 {
		return receipt, nativePublicInvalid("A producer lease and fencing token are required")
	}
	if request.Producer.RunnerID != "" && request.Producer.RunnerID != scope.credential.Runner.RunnerID {
		return receipt, nativePublicInvalid("Producer identity must match the authenticated lease owner")
	}
	files, patchBytes, err := normalizeAttemptDiff(scope, request)
	if err != nil {
		return receipt, err
	}
	if s.hostedShared() {
		for _, file := range files {
			if file.Patch != "" {
				return receipt, nativePublicInvalid("Shared diff bodies must arrive through object storage")
			}
		}
	}
	if checkOnly {
		return receipt, s.validateAttemptDiffTenant(ctx, scope, attemptID, request)
	}
	if err := s.validateAttemptDiffTenant(ctx, scope, attemptID, request); err != nil {
		return receipt, err
	}
	data, err := json.Marshal(files)
	if err != nil {
		return receipt, fmt.Errorf("encode attempt diff: %w", err)
	}
	preparedRef, _, err := attachment.PrepareArchive(string(scope.organization), 1, bytes.NewReader(data))
	if err != nil {
		return receipt, err
	}
	if err := s.checkAttemptDiffStorageAdmission(ctx, scope, preparedRef); err != nil {
		return receipt, err
	}
	store := s.runDetailStore()
	ref, err := store.StoreDetail(ctx, string(scope.organization), data)
	if err != nil {
		return receipt, fmt.Errorf("store attempt diff: %w", err)
	}
	if ref.Validate(string(scope.organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(data)) {
		return receipt, attachment.ErrInvalid
	}
	verified, err := store.ReadDetail(ctx, string(scope.organization), ref)
	if err != nil {
		return receipt, fmt.Errorf("verify attempt diff: %w", err)
	}
	if string(verified) != string(data) {
		return receipt, attachment.ErrInvalid
	}
	if preparedRef != ref {
		return receipt, attachment.ErrInvalid
	}
	postedBytes := diffPostedBytes(request.Files)
	if request.PostedBytes > 0 {
		postedBytes = request.PostedBytes
	}
	truncated := false
	for _, file := range files {
		truncated = truncated || file.Truncated
	}
	now := s.config.now().UTC()
	diffID, archiveID := newNativeID("diff"), newNativeID("archive")
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := resolveAttemptDiffTenant(ctx, w, scope, attemptID, request, now); err != nil {
			return err
		}
		latest, err := w.LockAttemptDiffGeneration(ctx, attemptID, string(request.Producer.LeaseID), scope.credential.ID, request.Generation.Source, request.Generation.ID)
		if err != nil {
			return err
		}
		if request.Generation.Seq <= latest {
			return attemptDiffStaleGeneration("Generation " + strconv.FormatInt(request.Generation.Seq, 10) + " is not ahead of the stored generation " + strconv.FormatInt(latest, 10))
		}
		item, err := resolveAttemptDiffTenant(ctx, w, scope, attemptID, request, s.config.now().UTC())
		if err != nil {
			return err
		}
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: archiveID, Reference: ref, CreatedAt: now})
		if err != nil {
			return err
		}
		if object.ID == archiveID {
			limit, err := s.attachmentQuotaLimit(ctx, w)
			if err != nil {
				return err
			}
			usage, err := w.AdjustCollaborationBytes(ctx, ref.CompressedSize, limit)
			if errors.Is(err, corestore.ErrNotFound) {
				return attachmentAccountingUnavailable()
			}
			if err != nil {
				return err
			}
			if !usage.Allowed {
				return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
			}
		}
		if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.organization, ArchiveID: object.ID, Kind: "attempt-diff", ID: diffID, CreatedAt: now}); err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, attemptID)
		if err != nil {
			return err
		}
		return w.CreateAttemptDiff(ctx, leasestore.AttemptDiff{
			OrganizationID: scope.organization, ID: diffID, AttemptID: attemptID, IssueID: attempt.IssueID,
			ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(item), Generation: request.Generation,
			Producer: tracker.DiffProducer{Kind: request.Producer.Kind, ID: attemptID, RunnerID: scope.credential.Runner.RunnerID, LeaseID: request.Producer.LeaseID, FencingToken: request.Producer.FencingToken},
			BaseSHA:  request.BaseSHA, HeadSHA: request.HeadSHA, FileCount: len(files), PatchBytes: patchBytes,
			PostedBytes: postedBytes, Truncated: truncated, ArchiveID: object.ID, CreatedAt: now,
		})
	})
	if err != nil {
		return receipt, err
	}
	return tracker.AttemptDiffReceipt{Accepted: true, DiffID: diffID, Generation: request.Generation, FileCount: len(files), PatchBytes: patchBytes, Truncated: truncated}, nil
}

func (s *Service) checkAttemptDiffStorageAdmission(ctx context.Context, scope nativeScope, ref attachment.ArchiveReference) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		limit, err := s.attachmentQuotaLimit(ctx, r)
		if err != nil {
			return err
		}
		used, err := r.CollaborationBytes(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return attachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if _, err := r.ArchiveObjectByReference(ctx, ref); err == nil {
			return nil
		} else if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if used > limit || ref.CompressedSize > limit-used {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
		}
		return nil
	})
}

func (s *Service) validateAttemptDiffTenant(ctx context.Context, scope nativeScope, attemptID string, request tracker.AttemptDiffRequest) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := resolveAttemptDiffTenant(ctx, r, scope, attemptID, request, s.config.now().UTC())
		if err != nil {
			return err
		}
		latest, err := r.LatestAttemptDiffSequence(ctx, attemptID, request.Generation.Source, request.Generation.ID)
		if err != nil {
			return err
		}
		if request.Generation.Seq <= latest {
			return attemptDiffStaleGeneration("Generation " + strconv.FormatInt(request.Generation.Seq, 10) + " is not ahead of the stored generation " + strconv.FormatInt(latest, 10))
		}
		return nil
	})
}

func resolveAttemptDiffTenant(ctx context.Context, r tenantstore.Reader, scope nativeScope, attemptID string, request tracker.AttemptDiffRequest, now time.Time) (string, error) {
	if scope.credential.Runner.RunnerID == "" || scope.credential.Runner.OrganizationID != scope.organization {
		return "", nativeStaleExecution("The authenticated runner does not own the producer lease")
	}
	credential, err := r.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", runnerUnauthorized()
		}
		return "", err
	}
	if credential.OrganizationID != scope.organization || credential.Kind != "service" || credential.Scope != string(apiScopeWorker) || !credential.Valid(scope.credential.Hash, now) {
		return "", runnerUnauthorized()
	}
	identity, err := r.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", runnerUnauthorized()
		}
		return "", err
	}
	if identity.OrganizationID != scope.organization || identity.RunnerID != scope.credential.Runner.RunnerID || identity.TokenID != credential.ID || identity.RemovedAt != nil || identity.State == "disabled" || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
		return "", runnerUnauthorized()
	}
	projects, err := r.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return "", err
	}
	if !slices.Contains(projects, scope.project) {
		return "", nativeNotFound()
	}
	lease, err := r.Lease(ctx, string(request.Producer.LeaseID))
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", nativeNotFound()
		}
		return "", err
	}
	attempt, err := r.Attempt(ctx, attemptID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", nativeStaleExecution("The attempt is not the running owner of the producer lease")
		}
		return "", err
	}
	if attempt.Status != "running" || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != int64(request.Producer.FencingToken) || attempt.IssueID != lease.IssueID || lease.FencingToken != int64(request.Producer.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return "", nativeStaleExecution("The producer lease is no longer current")
	}
	leaseIdentity, err := r.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return "", err
	}
	if leaseIdentity.RunnerID != scope.credential.Runner.RunnerID || leaseIdentity.MachineID != scope.credential.Runner.MachineID || lease.MachineID != string(leaseIdentity.MachineID) {
		return "", nativeStaleExecution("The producer lease belongs to a different runner session")
	}
	item, err := r.WorkItemByID(ctx, scope.project, lease.IssueID)
	if err != nil {
		return "", err
	}
	return string(item.NativeID), nil
}

func (s *Service) readAttemptDiffTenant(ctx context.Context, scope nativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	var diff leasestore.AttemptDiff
	var archive artifactstore.ArchiveObject
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if seq > 0 {
			diff, err = r.AttemptDiff(ctx, scope.project, attemptID, tracker.DiffGeneration{Source: source, Seq: seq})
		} else if item == "" {
			diff, err = r.LatestAttemptDiff(ctx, scope.project, attemptID, source)
		} else {
			diff, err = r.LatestAttemptDiff(ctx, scope.project, attemptID, source)
		}
		if err != nil {
			return err
		}
		if item != "" && string(diff.WorkItemID) != item {
			return nativeNotFound()
		}
		if _, err := attemptDiffReadableWorkItem(ctx, r, scope, diff.WorkItemID); err != nil {
			return err
		}
		archive, err = r.ArchiveObject(ctx, diff.ArchiveID)
		return err
	})
	if err != nil {
		return tracker.AttemptDiff{}, err
	}
	return s.loadTenantAttemptDiff(ctx, scope, diff, archive)
}

func (s *Service) readWorkItemDiffTenant(ctx context.Context, scope nativeScope, item, source string) (tracker.WorkItemDiff, error) {
	var diff leasestore.AttemptDiff
	var archive artifactstore.ArchiveObject
	foundDiff := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := attemptDiffReadableWorkItem(ctx, r, scope, tracker.NativeWorkItemID(item)); err != nil {
			return err
		}
		var err error
		diff, err = r.LatestWorkItemAttemptDiff(ctx, scope.project, tracker.NativeWorkItemID(item), source)
		if err != nil {
			return err
		}
		foundDiff = true
		archive, err = r.ArchiveObject(ctx, diff.ArchiveID)
		return err
	})
	if !foundDiff && (errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows)) {
		return tracker.WorkItemDiff{}, nil
	}
	if err != nil {
		return tracker.WorkItemDiff{}, err
	}
	loaded, err := s.loadTenantAttemptDiff(ctx, scope, diff, archive)
	if err != nil {
		return tracker.WorkItemDiff{}, err
	}
	return tracker.WorkItemDiff{Diff: &loaded}, nil
}

func attemptDiffReadableWorkItem(ctx context.Context, r tenantstore.Reader, scope nativeScope, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	project, err := r.Project(ctx, scope.project)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || err == nil && project.DeletedAt != nil {
		return corestore.WorkItem{}, nativeNotFound()
	}
	if err != nil {
		return corestore.WorkItem{}, err
	}
	item, err := r.WorkItem(ctx, scope.project, id)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || err == nil && !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
		return corestore.WorkItem{}, nativeNotFound()
	}
	if err != nil {
		return corestore.WorkItem{}, err
	}
	return item, nil
}

func (s *Service) loadTenantAttemptDiff(ctx context.Context, scope nativeScope, diff leasestore.AttemptDiff, archive artifactstore.ArchiveObject) (tracker.AttemptDiff, error) {
	if s.runDetailStore() == nil {
		return tracker.AttemptDiff{}, nativeRunEventStorageUnavailable()
	}
	data, err := s.runDetailStore().ReadDetail(ctx, string(scope.organization), archive.Reference)
	if err != nil {
		return tracker.AttemptDiff{}, err
	}
	var files []tracker.AttemptDiffFile
	if int64(len(data)) != archive.Reference.UncompressedSize {
		return tracker.AttemptDiff{}, artifact.ErrIntegrity
	}
	if err := json.Unmarshal(data, &files); err != nil {
		return tracker.AttemptDiff{}, fmt.Errorf("decode attempt diff: %w", err)
	}
	files = expireAttemptDiffBodies(files, diff.ManifestExpired)
	return tracker.AttemptDiff{ID: diff.ID, AttemptID: diff.AttemptID, Producer: diff.Producer, Generation: diff.Generation, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: files, FileCount: diff.FileCount, PatchBytes: diff.PatchBytes, Truncated: diff.Truncated, CreatedAt: diff.CreatedAt}, nil
}

func expireAttemptDiffBodies(files []tracker.AttemptDiffFile, expired bool) []tracker.AttemptDiffFile {
	if !expired {
		return files
	}
	for index := range files {
		if files[index].PatchBody != nil {
			files[index].PatchBody = nil
			files[index].PatchExpired = true
		}
	}
	return files
}
