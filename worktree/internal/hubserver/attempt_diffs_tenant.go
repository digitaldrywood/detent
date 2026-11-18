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
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) postAttemptDiffTenant(ctx context.Context, scope hubscope.NativeScope, attemptID string, request tracker.AttemptDiffRequest, checkOnly bool) (tracker.AttemptDiffReceipt, error) {
	var receipt tracker.AttemptDiffReceipt
	if s.config.TenantRepository == nil || s.runDetailStore() == nil {
		return receipt, hubapi.NativeRunEventStorageUnavailable()
	}
	if request.Producer.Kind != tracker.DiffSourceAttempt {
		return receipt, hubapi.NativeStaleExecution("Only the attempt's own lease may write this diff until a workspace session owns it")
	}
	if request.Producer.ID != "" && request.Producer.ID != attemptID {
		return receipt, hubapi.NativeStaleExecution("An attempt producer may only write its own attempt's diff")
	}
	if strings.TrimSpace(string(request.Producer.LeaseID)) == "" || request.Producer.FencingToken <= 0 {
		return receipt, hubapi.NativePublicInvalid("A producer lease and fencing token are required")
	}
	if request.Producer.RunnerID != "" && request.Producer.RunnerID != scope.Credential.Runner.RunnerID {
		return receipt, hubapi.NativePublicInvalid("Producer identity must match the authenticated lease owner")
	}
	files, patchBytes, err := normalizeAttemptDiff(scope, request)
	if err != nil {
		return receipt, err
	}
	if s.hostedShared() {
		for _, file := range files {
			if file.Patch != "" {
				return receipt, hubapi.NativePublicInvalid("Shared diff bodies must arrive through object storage")
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
	preparedRef, _, err := attachment.PrepareArchive(string(scope.Organization), 1, bytes.NewReader(data))
	if err != nil {
		return receipt, err
	}
	if err := s.checkAttemptDiffStorageAdmission(ctx, scope, preparedRef); err != nil {
		return receipt, err
	}
	store := s.runDetailStore()
	ref, err := store.StoreDetail(ctx, string(scope.Organization), data)
	if err != nil {
		return receipt, fmt.Errorf("store attempt diff: %w", err)
	}
	if ref.Validate(string(scope.Organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(data)) {
		return receipt, attachment.ErrInvalid
	}
	verified, err := store.ReadDetail(ctx, string(scope.Organization), ref)
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
	now := s.config.Clock().UTC()
	diffID, archiveID := hubapi.NewID("diff"), hubapi.NewID("archive")
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := resolveAttemptDiffTenant(ctx, w, scope, attemptID, request, now); err != nil {
			return err
		}
		latest, err := w.LockAttemptDiffGeneration(ctx, attemptID, string(request.Producer.LeaseID), scope.Credential.ID, request.Generation.Source, request.Generation.ID)
		if err != nil {
			return err
		}
		if request.Generation.Seq <= latest {
			return attemptDiffStaleGeneration("Generation " + strconv.FormatInt(request.Generation.Seq, 10) + " is not ahead of the stored generation " + strconv.FormatInt(latest, 10))
		}
		item, err := resolveAttemptDiffTenant(ctx, w, scope, attemptID, request, s.config.Clock().UTC())
		if err != nil {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, w)
		if err != nil {
			return err
		}
		if err := w.LockArchiveAccounting(ctx); err != nil {
			return err
		}
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: archiveID, Reference: ref, CreatedAt: now})
		if err != nil {
			return err
		}
		usage, err := w.ChargeArchiveObject(ctx, object.ID, limit)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "attempt-diff", ID: diffID, CreatedAt: now}); err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if err := w.CreateAttemptDiff(ctx, leasestore.AttemptDiff{
			OrganizationID: scope.Organization, ID: diffID, AttemptID: attemptID, IssueID: attempt.IssueID,
			ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), Generation: request.Generation,
			Producer: tracker.DiffProducer{Kind: request.Producer.Kind, ID: attemptID, RunnerID: scope.Credential.Runner.RunnerID, LeaseID: request.Producer.LeaseID, FencingToken: request.Producer.FencingToken},
			BaseSHA:  request.BaseSHA, HeadSHA: request.HeadSHA, FileCount: len(files), PatchBytes: patchBytes,
			PostedBytes: postedBytes, Truncated: truncated, ArchiveID: object.ID, CreatedAt: now,
		}); err != nil {
			return err
		}
		keys := make([]string, 0, len(files))
		for _, file := range files {
			if file.PatchBody != nil {
				keys = append(keys, file.PatchBody.Key)
			}
		}
		return w.IndexAttemptDiffBodies(ctx, scope.Project, diffID, keys)
	})
	if err != nil {
		return receipt, err
	}
	return tracker.AttemptDiffReceipt{Accepted: true, DiffID: diffID, Generation: request.Generation, FileCount: len(files), PatchBytes: patchBytes, Truncated: truncated}, nil
}

func (s *Service) checkAttemptDiffStorageAdmission(ctx context.Context, scope hubscope.NativeScope, ref attachment.ArchiveReference) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		limit, err := s.attachmentQuotaLimit(ctx, r)
		if err != nil {
			return err
		}
		used, err := r.CollaborationBytes(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
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
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
		}
		return nil
	})
}

func (s *Service) validateAttemptDiffTenant(ctx context.Context, scope hubscope.NativeScope, attemptID string, request tracker.AttemptDiffRequest) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := resolveAttemptDiffTenant(ctx, r, scope, attemptID, request, s.config.Clock().UTC())
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

func resolveAttemptDiffTenant(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, attemptID string, request tracker.AttemptDiffRequest, now time.Time) (string, error) {
	if scope.Credential.Runner.RunnerID == "" || scope.Credential.Runner.OrganizationID != scope.Organization {
		return "", hubapi.NativeStaleExecution("The authenticated runner does not own the producer lease")
	}
	identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", hubscope.RunnerUnauthorized()
		}
		return "", err
	}
	credential, err := r.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", hubscope.RunnerUnauthorized()
		}
		return "", err
	}
	if credential.OrganizationID != scope.Organization || credential.Kind != "service" || credential.Scope != string(hubapi.APIScopeWorker) || !credential.Valid(scope.Credential.Hash, now) {
		return "", hubscope.RunnerUnauthorized()
	}
	if identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.TokenID != credential.ID || identity.RemovedAt != nil || identity.State == "disabled" || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
		return "", hubscope.RunnerUnauthorized()
	}
	projects, err := r.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return "", err
	}
	if !slices.Contains(projects, scope.Project) {
		return "", hubapi.NativeNotFound()
	}
	lease, err := r.Lease(ctx, string(request.Producer.LeaseID))
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", hubapi.NativeNotFound()
		}
		return "", err
	}
	attempt, err := r.Attempt(ctx, attemptID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", hubapi.NativeStaleExecution("The attempt is not the running owner of the producer lease")
		}
		return "", err
	}
	if attempt.Status != "running" || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != int64(request.Producer.FencingToken) || attempt.IssueID != lease.IssueID || lease.FencingToken != int64(request.Producer.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return "", hubapi.NativeStaleExecution("The producer lease is no longer current")
	}
	leaseIdentity, err := r.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return "", err
	}
	if leaseIdentity.RunnerID != scope.Credential.Runner.RunnerID || leaseIdentity.MachineID != scope.Credential.Runner.MachineID || lease.MachineID != string(leaseIdentity.MachineID) {
		return "", hubapi.NativeStaleExecution("The producer lease belongs to a different runner session")
	}
	item, err := r.WorkItemByID(ctx, scope.Project, lease.IssueID)
	if err != nil {
		return "", err
	}
	return string(item.NativeID), nil
}

func (s *Service) readAttemptDiffTenant(ctx context.Context, scope hubscope.NativeScope, item, attemptID, source string, seq int64) (tracker.AttemptDiff, error) {
	var diff leasestore.AttemptDiff
	var archive artifactstore.ArchiveObject
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if seq > 0 {
			diff, err = r.AttemptDiff(ctx, scope.Project, attemptID, tracker.DiffGeneration{Source: source, Seq: seq})
		} else if item == "" {
			diff, err = r.LatestAttemptDiff(ctx, scope.Project, attemptID, source)
		} else {
			diff, err = r.LatestAttemptDiff(ctx, scope.Project, attemptID, source)
		}
		if err != nil {
			return err
		}
		if item != "" && string(diff.WorkItemID) != item {
			return hubapi.NativeNotFound()
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

func (s *Service) readWorkItemDiffTenant(ctx context.Context, scope hubscope.NativeScope, item, source string) (tracker.WorkItemDiff, error) {
	var diff leasestore.AttemptDiff
	var archive artifactstore.ArchiveObject
	foundDiff := false
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := attemptDiffReadableWorkItem(ctx, r, scope, tracker.NativeWorkItemID(item)); err != nil {
			return err
		}
		var err error
		diff, err = r.LatestWorkItemAttemptDiff(ctx, scope.Project, tracker.NativeWorkItemID(item), source)
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

func attemptDiffReadableWorkItem(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	project, err := r.Project(ctx, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || err == nil && project.DeletedAt != nil {
		return corestore.WorkItem{}, hubapi.NativeNotFound()
	}
	if err != nil {
		return corestore.WorkItem{}, err
	}
	item, err := r.WorkItem(ctx, scope.Project, id)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || err == nil && !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
		return corestore.WorkItem{}, hubapi.NativeNotFound()
	}
	if err != nil {
		return corestore.WorkItem{}, err
	}
	return item, nil
}

func (s *Service) loadTenantAttemptDiff(ctx context.Context, scope hubscope.NativeScope, diff leasestore.AttemptDiff, archive artifactstore.ArchiveObject) (tracker.AttemptDiff, error) {
	if s.runDetailStore() == nil {
		return tracker.AttemptDiff{}, hubapi.NativeRunEventStorageUnavailable()
	}
	data, err := s.runDetailStore().ReadDetail(ctx, string(scope.Organization), archive.Reference)
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
