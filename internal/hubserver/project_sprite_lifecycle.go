package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spriteLifecycleRequest struct {
	TokenProject    tracker.ProjectID   `json:"token_project_id"`
	Provider        string              `json:"provider_organization"`
	Actor           string              `json:"actor"`
	Settings        spritePoolSettings  `json:"settings"`
	EnrollmentToken hubsecrets.Envelope `json:"enrollment_token"`
	Version         string              `json:"version"`
	State           string              `json:"state,omitempty"`
}

type spriteLifecycleReceipt struct {
	ResourceID string `json:"resource_id"`
	Log        string `json:"log,omitempty"`
}

type spriteBootstrapExitError struct{ code int }

func (e spriteBootstrapExitError) Error() string {
	return fmt.Sprintf("sprite bootstrap exited with code %d", e.code)
}

func (s *Service) rejectSpriteOperation(ctx context.Context, op postgres.SpriteOperation, log string, remoteCreated bool) error {
	return s.rejectSpriteOperationWithRetry(ctx, op, log, remoteCreated, true)
}

func (s *Service) rejectSpriteOperationWithRetry(ctx context.Context, op postgres.SpriteOperation, log string, remoteCreated, retry bool) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	key, err := s.spriteLifecycleArchive(cleanup, op.OrganizationID, spriteLifecycleReceipt{Log: log})
	if err != nil {
		return err
	}
	var member spritePoolMember
	err = s.config.TenantRepository.WriteTenant(cleanup, op.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		op.Outcome = "rejected"
		op.ReceiptObjectKey = &key
		op.UpdatedAt = s.config.now()
		op.NextAttemptAt = op.UpdatedAt
		if retry {
			op.NextAttemptAt = spriteOperationRetryAt(op, op.UpdatedAt)
		}
		if err := owner.UpdateSpriteOperation(cleanup, op, op.Attempts); err != nil {
			return err
		}
		current, err := owner.SpriteMember(cleanup, op.Name)
		if err != nil {
			return err
		}
		row := spriteMembershipRecord(current)
		row.State = "deleted"
		if remoteCreated {
			row.State = "deleting"
		}
		if err := w.UpdateRunnerSpriteMembership(cleanup, row); err != nil {
			return err
		}
		enrollment, err := w.RunnerEnrollment(cleanup, row.EnrollmentID)
		if err != nil {
			return err
		}
		if enrollment.RedeemedAt == nil && enrollment.RevokedAt == nil {
			if err := w.RevokeRunnerEnrollment(cleanup, row.EnrollmentID, op.UpdatedAt); err != nil {
				return err
			}
		}
		member = spritePoolMember{Name: row.Name, EnrollmentID: row.EnrollmentID, AllocationProjectID: row.AllocationProjectID}
		if retry && row.AllocationProjectID != "" {
			allocation, err := owner.SpriteAllocation(cleanup, row.AllocationProjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if allocation.Revision == row.AllocationRevision {
				return owner.RecordSpriteProvisionFailure(cleanup, row.AllocationProjectID, row.AllocationRevision)
			}
		}
		return nil
	})
	if err != nil || !remoteCreated {
		return err
	}
	return s.retirePoolSpriteRecord(cleanup, nativeScope{organization: op.OrganizationID, project: op.ProjectID}, member, ctx.Err() == nil)
}

func (s *Service) spriteLifecycleArchive(ctx context.Context, organization tracker.OrganizationID, value any) (string, error) {
	store := s.runDetailStore()
	if store == nil {
		return "", artifact.ErrStorage
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	ref, err := store.StoreDetail(ctx, string(organization), raw)
	return ref.Key, err
}

func (s *Service) spriteLifecyclePayload(ctx context.Context, op postgres.SpriteOperation) (spriteLifecycleRequest, error) {
	var request spriteLifecycleRequest
	reader, ok := s.runDetailStore().(attachment.CanonicalDetailStore)
	if !ok || op.RequestObjectKey == nil {
		return request, artifact.ErrStorage
	}
	raw, err := reader.ReadCanonicalDetail(ctx, string(op.OrganizationID), "sprite-operation", *op.RequestObjectKey)
	if err != nil {
		return request, err
	}
	if len(raw) > 131072 || json.Unmarshal(raw, &request) != nil || request.Actor == "" {
		return request, artifact.ErrIntegrity
	}
	return request, nil
}

func (s *Service) spriteMemberLogRecord(ctx context.Context, organization tracker.OrganizationID, name string) (string, error) {
	var operations []postgres.SpriteOperation
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		var err error
		operations, err = owner.SpriteMemberOperations(ctx, name)
		return err
	})
	if err != nil {
		return "", err
	}
	var log strings.Builder
	for _, operation := range operations {
		reader, ok := s.runDetailStore().(attachment.CanonicalDetailStore)
		if !ok {
			return "", artifact.ErrStorage
		}
		raw, err := reader.ReadCanonicalDetail(ctx, string(organization), "sprite-operation", *operation.ReceiptObjectKey)
		if err != nil {
			return "", err
		}
		var receipt spriteLifecycleReceipt
		if len(raw) > 16384 || json.Unmarshal(raw, &receipt) != nil {
			return "", artifact.ErrIntegrity
		}
		if log.Len()+len(receipt.Log) > 8500 {
			return "", artifact.ErrIntegrity
		}
		log.WriteString(receipt.Log)
	}
	return log.String(), nil
}

func (s *Service) spriteLifecycleAuthority(ctx context.Context, r tenantstore.Reader, organization tracker.OrganizationID, actor string) (nativeScope, error) {
	scope := nativeScope{organization: organization}
	credential, err := r.AccessCredential(ctx, actor)
	if err != nil {
		return scope, err
	}
	if !credential.Valid(credential.Hash, s.config.now()) {
		return scope, accessstore.ErrDenied
	}
	if credential.Kind == "service" && credential.Scope == string(apiScopeAdmin) && !credential.NativeOnly {
		scope.credential = apiCredential{ID: credential.ID, Hash: credential.Hash, Scope: apiScope(credential.Scope)}
		return scope, nil
	}
	actors, ok := r.(receiptstore.ActorReader)
	if !ok {
		return scope, accessstore.ErrTransactionRequired
	}
	member, err := actors.ReceiptMember(ctx, actor)
	if err != nil {
		return scope, err
	}
	if !member.Active || member.Role != "owner" && member.Role != "admin" {
		return scope, accessstore.ErrDenied
	}
	scope.credential = apiCredential{ID: credential.ID, Hash: credential.Hash, Scope: apiScope(credential.Scope), HostedRole: member.Role, HostedPrincipal: member.PrincipalID}
	return scope, nil
}

func (s *Service) spritePoolLifecycleScope(ctx context.Context, organization tracker.OrganizationID) (nativeScope, error) {
	var scope nativeScope
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		pool, err := owner.SpritePool(ctx)
		if err != nil {
			return err
		}
		scope, err = s.spriteLifecycleAuthority(ctx, r, organization, pool.ConfiguredBy)
		return err
	})
	return scope, err
}

func (s *Service) createPoolSpriteRecord(ctx context.Context, scope nativeScope, settings spritePoolSettings, allowOrganization bool, name string) (bool, error) {
	scope, err := s.spritePoolLifecycleScope(ctx, scope.organization)
	if err != nil {
		return false, err
	}
	requestedRevision := settings.Revision
	settings, _, err = s.spritePoolSettingsRecord(ctx, scope)
	if err != nil || settings.MaxRunners == 0 {
		return false, err
	}
	if requestedRevision != settings.Revision {
		return false, nil
	}
	var active int64
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		var err error
		active, err = owner.SpritePoolMemberCount(ctx)
		return err
	}); err != nil {
		return false, err
	}
	if active >= int64(settings.MaxRunners) {
		return false, nil
	}
	version := "v" + strings.TrimPrefix(s.config.Version, "v")
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return false, nativePublicInvalid("Sprite provisioning requires a Hub built from a pinned release")
	}
	placement, err := s.spritePlacementRecord(ctx, scope)
	if err != nil {
		return false, err
	}
	secretScope, provider, _, err := s.resolveSpritesSecret(ctx, nil, scope)
	if err != nil {
		return false, err
	}
	token, err := s.config.generateToken()
	if err != nil {
		return false, err
	}
	enrollmentID := newNativeID("enrollment")
	sealed, err := s.config.SecretKeys.Seal([]byte(token), secretAAD(string(scope.organization), enrollmentID, "sprite-enrollment"))
	if err != nil {
		return false, err
	}
	requestKey, err := s.spriteLifecycleArchive(ctx, scope.organization, spriteLifecycleRequest{Actor: scope.credential.ID, Settings: settings, EnrollmentToken: sealed, Version: version})
	if err != nil {
		return false, err
	}
	decision, err := checkMonthlyBudget(ctx, nil, scope, budget.CostExposure{SpriteInfrastructure: true}, false, s.config.now(), s.database.billingRepository)
	if err != nil || !decision.Allowed {
		return false, err
	}
	now := s.config.now()
	var operation postgres.SpriteOperation
	reserved := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		pool, err := owner.LockSpritePool(ctx)
		if err != nil {
			return err
		}
		if _, err := s.spriteLifecycleAuthority(ctx, w, scope.organization, pool.ConfiguredBy); err != nil {
			return err
		}
		if pool.Revision != requestedRevision {
			return nil
		}
		count, err := owner.SpritePoolMemberCount(ctx)
		if err != nil {
			return err
		}
		if count >= int64(pool.MaxRunners) {
			return nil
		}
		if _, err := owner.SpriteMember(ctx, name); err == nil {
			return errSpriteNameTaken
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		secret, err := w.AccessSecret(ctx, secretScope.project, flySpritesToken)
		if err != nil {
			return err
		}
		if secret.OrganizationSlug != provider {
			return corestore.ErrRevisionConflict
		}
		provisionAttempt := func(project tracker.ProjectID, revision int64) (string, int64, bool, error) {
			source, err := nativeCommandHash(struct {
				Pool            int64
				Allocation      int64
				SecretUpdatedAt time.Time
				Version         string
			}{pool.Revision, revision, secret.UpdatedAt, version})
			if err != nil {
				return "", 0, false, err
			}
			previous, err := owner.SpriteProvisionSourceFailure(ctx, project, source)
			if errors.Is(err, corestore.ErrNotFound) {
				return source, 0, true, nil
			}
			if err != nil {
				return "", 0, false, err
			}
			return source, previous.Attempts, !previous.NextAttemptAt.After(now), nil
		}
		var source string
		var attempts int64
		const allocationPageSize = 100
		begin := postgres.SpriteAllocationCursor{RequestedAt: time.Unix(0, 0).UTC()}
		stored := postgres.SpriteAllocationCursor{RequestedAt: pool.ProvisionAfterRequestedAt, ProjectID: tracker.ProjectID(pool.ProvisionAfterProjectID)}
		page, err := owner.SpriteProvisionQueue(ctx, stored, allocationPageSize+1)
		if err != nil {
			return err
		}
		if len(page) == 0 && stored.ProjectID != "" {
			page, err = owner.SpriteProvisionQueue(ctx, begin, allocationPageSize+1)
			if err != nil {
				return err
			}
		}
		more := len(page) > allocationPageSize
		allocations := page[:min(len(page), allocationPageSize)]
		var revision int64
		floor := settings.MinRunners
		for _, allocation := range allocations {
			active, err := owner.SpriteActiveMemberCount(ctx, allocation.ProjectID)
			if err != nil {
				return err
			}
			if active >= int64(allocation.DesiredCount) {
				continue
			}
			valid, err := owner.SpriteAllocationAuthority(ctx, allocation.ProjectID, now)
			if err != nil {
				return err
			}
			if !valid {
				continue
			}
			allocationScope := scope
			allocationScope.project = allocation.ProjectID
			budgetInput, err := readBillingMonthlyBudgetInput(ctx, w, allocationScope, now)
			if err != nil {
				return err
			}
			budgetInput.Exposure = budget.CostExposure{SpriteInfrastructure: true}
			decision, err := billingstore.EvaluateMonthlyBudget(budgetInput)
			if err != nil {
				return err
			}
			if !decision.Allowed {
				continue
			}
			candidateSource, candidateAttempts, due, err := provisionAttempt(allocation.ProjectID, allocation.Revision)
			if err != nil {
				return err
			}
			if !due {
				continue
			}
			scope.project = allocation.ProjectID
			floor = int(allocation.DesiredCount)
			revision = allocation.Revision
			source, attempts = candidateSource, candidateAttempts
			break
		}
		next := begin
		if scope.project == "" && more {
			last := allocations[len(allocations)-1]
			next = postgres.SpriteAllocationCursor{RequestedAt: last.RequestedAt, ProjectID: last.ProjectID}
		}
		if next.ProjectID != stored.ProjectID || !next.RequestedAt.Equal(stored.RequestedAt) {
			if err := owner.SetSpriteProvisionCursor(ctx, next); err != nil {
				return err
			}
		}
		if scope.project == "" && (more || !allowOrganization) {
			return nil
		}
		active, err := owner.SpriteActiveMemberCount(ctx, scope.project)
		if err != nil {
			return err
		}
		input := spriteScaleInput{Floor: floor, Ceiling: int(active) + int(pool.MaxRunners) - int(count), Count: int(active), Depth: placement.SpriteTarget, Free: placement.SpriteFree, Pending: placement.Pending, CreateLimit: placement.ProviderFree}
		if scope.project != "" {
			input.Depth, input.Free, input.Pending = 0, 0, 0
			input.Ceiling = min(input.Ceiling, floor)
		} else if placement.Policy.Mode == "local_first" {
			input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
		}
		budgetInput, err := readBillingMonthlyBudgetInput(ctx, w, scope, now)
		if err != nil {
			return err
		}
		budgetInput.Exposure = budget.CostExposure{SpriteInfrastructure: true}
		budgetDecision, err := billingstore.EvaluateMonthlyBudget(budgetInput)
		if err != nil || !budgetDecision.Allowed {
			return err
		}
		if decideSpriteScale(input).Create == 0 {
			return nil
		}
		if source == "" {
			var due bool
			source, attempts, due, err = provisionAttempt(scope.project, revision)
			if err != nil || !due {
				return err
			}
		}
		enrollment := runnerstore.Enrollment{OrganizationID: scope.organization, ID: enrollmentID, TokenHash: apikey.HashToken(token), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, CreatedAt: now, ExpiresAt: now.Add(runnerauth.MaxEnrollmentTTL), CreatedBy: scope.credential.ID}
		if scope.project != "" {
			enrollment.Scope = "projects"
			enrollment.ProjectIDs = []tracker.ProjectID{scope.project}
		}
		if err := w.CreateRunnerEnrollment(ctx, enrollment); err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, w, now, enrollmentID, false, false); err != nil {
			return err
		}
		if err := w.CreateRunnerSpriteMembership(ctx, runnerstore.SpriteMembership{OrganizationID: scope.organization, Name: name, ProviderOrganization: provider, TokenProjectID: secretScope.project, EnrollmentID: enrollmentID, State: "bootstrapping", IdleSince: now, CreatedAt: now, AllocationProjectID: scope.project, AllocationRevision: revision}); err != nil {
			return err
		}
		operation, err = owner.CreateSpriteOperation(ctx, postgres.SpriteOperation{OrganizationID: scope.organization, IdempotencyKey: "provision:" + enrollmentID, Name: name, Operation: "provision", Attempts: attempts, SourceRevision: source, RequestObjectKey: &requestKey, TargetEnrollmentID: enrollmentID, ProjectID: scope.project, CreatedAt: now, NextAttemptAt: now})
		reserved = err == nil
		return err
	})
	if err != nil || !reserved {
		return reserved, err
	}
	for _, phase := range []string{"provision", "bootstrap", "checkpoint"} {
		if phase != "provision" {
			err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				owner, ok := r.(postgres.SpriteReader)
				if !ok {
					return accessstore.ErrTransactionRequired
				}
				var err error
				operation, err = owner.SpriteOperation(ctx, phase+":"+enrollmentID)
				return err
			})
			if err != nil {
				return true, err
			}
		}
		if err := s.runSpriteOperation(ctx, operation); err != nil {
			return true, err
		}
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			var err error
			operation, err = owner.SpriteOperation(ctx, operation.IdempotencyKey)
			return err
		}); err != nil {
			return true, err
		}
		if operation.Outcome == "rejected" {
			return true, nil
		}
		if operation.Outcome != "committed" {
			return true, errSpritesValidation
		}
	}
	return true, nil
}

func spriteMembershipRecord(row postgres.SpriteMember) runnerstore.SpriteMembership {
	return runnerstore.SpriteMembership{OrganizationID: row.OrganizationID, Name: row.Name, ProviderOrganization: row.ProviderOrganization, TokenProjectID: tracker.ProjectID(row.TokenProjectID), EnrollmentID: row.EnrollmentID, State: row.State, BootstrapLog: row.BootstrapLog, IdleSince: row.IdleSince, CreatedAt: row.CreatedAt, AllocationProjectID: tracker.ProjectID(row.AllocationProjectID), AllocationRevision: row.AllocationRevision}
}

func (s *Service) spriteOperationToken(ctx context.Context, op postgres.SpriteOperation, request spriteLifecycleRequest) ([]byte, error) {
	var scope nativeScope
	var provider string
	err := s.config.TenantRepository.ReadTenant(ctx, op.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if op.Operation == "wake" {
			project, err := r.ProjectRecord(ctx, op.ProjectID)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
				return state.Name == request.State && state.Dispatchable && !state.Terminal
			}) {
				return accessstore.ErrDenied
			}
			credential, err := r.AccessCredential(ctx, request.Actor)
			if err != nil {
				return err
			}
			if !credential.Valid(credential.Hash, s.config.now()) {
				return accessstore.ErrDenied
			}
			if _, err := r.AuthorizeAccess(ctx, accessstore.Authority{CredentialID: credential.ID, Hash: credential.Hash, ProjectID: op.ProjectID, Write: true, UserID: credential.UserID, MembershipID: credential.MembershipID}, s.config.now()); err != nil {
				return err
			}
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			allowed, err := owner.SpriteWakeTarget(ctx, op.TargetEnrollmentID, op.Name, op.ProjectID, s.config.now())
			if err != nil {
				return err
			}
			if !allowed {
				return accessstore.ErrDenied
			}
			enrollment, err := r.RunnerEnrollment(ctx, op.TargetEnrollmentID)
			if err != nil {
				return err
			}
			identity, err := r.RunnerIdentity(ctx, enrollment.RunnerID)
			if err != nil {
				return err
			}
			if s.config.now().Sub(identity.LastHeartbeatAt) < spriteRunnerIdle || identity.LastHeartbeatAt.After(op.CreatedAt) {
				return accessstore.ErrDenied
			}
			allowed, err = owner.SpriteRunnerAllowed(ctx, identity.RunnerID, op.ProjectID, s.config.now())
			if err != nil {
				return err
			}
			if !allowed {
				return accessstore.ErrDenied
			}
			scope = nativeScope{organization: op.OrganizationID, project: request.TokenProject, credential: apiCredential{ID: credential.ID, Hash: credential.Hash, Scope: apiScope(credential.Scope)}}
			provider = request.Provider
			return nil
		}
		actor := request.Actor
		if op.Operation == "delete" {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			pool, err := owner.SpritePool(ctx)
			if err != nil {
				return err
			}
			actor = pool.ConfiguredBy
		}
		scope, err = s.spriteLifecycleAuthority(ctx, r, op.OrganizationID, actor)
		if err != nil {
			return err
		}
		scope.project = op.ProjectID
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		if op.ProjectID != "" && op.Operation != "delete" && op.Operation != "wake" {
			valid, err := owner.SpriteAllocationAuthority(ctx, op.ProjectID, s.config.now())
			if err != nil {
				return err
			}
			if !valid {
				return accessstore.ErrDenied
			}
		}
		member, err := owner.SpriteMember(ctx, op.Name)
		if err != nil {
			return err
		}
		provider = member.ProviderOrganization
		scope.project = tracker.ProjectID(member.TokenProjectID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	resolved, slug, envelope, err := s.resolveSpritesSecret(ctx, nil, scope)
	if err != nil {
		return nil, err
	}
	if slug != provider {
		return nil, errSpritesValidation
	}
	if err := s.auditSecretUse(ctx, resolved, envelope.Version); err != nil {
		return nil, err
	}
	return s.config.SecretKeys.Open(envelope, secretAAD(string(resolved.organization), string(resolved.project), flySpritesToken))
}

func (s *Service) spriteWakeSecretRecord(ctx context.Context, scope nativeScope, name string) (nativeScope, string, hubsecrets.Envelope, error) {
	provider := ""
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		member, err := owner.SpriteMember(ctx, name)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if member.State != "enrolled" {
			return corestore.ErrNotFound
		}
		scope.project = tracker.ProjectID(member.TokenProjectID)
		provider = member.ProviderOrganization
		return nil
	})
	if err != nil {
		return scope, "", hubsecrets.Envelope{}, err
	}
	resolved, slug, envelope, err := s.resolveSpritesSecret(ctx, nil, scope)
	if err == nil && provider != "" && slug != provider {
		err = corestore.ErrNotFound
	}
	return resolved, slug, envelope, err
}

func (s *Service) spriteWakeFactsRecord(ctx context.Context, organization tracker.OrganizationID, runnerID string) (machineSpriteFacts, error) {
	var facts machineSpriteFacts
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		routing, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		record, err := routing.RunnerRoutingRecord(ctx, runnerID, s.config.now())
		if err != nil {
			return err
		}
		if record.Record.Machine.SpriteName != "" {
			facts.Name = new(record.Record.Machine.SpriteName)
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		operation, err := owner.SpriteEnrollmentWake(ctx, record.Record.Identity.EnrollmentID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if operation.Outcome == "committed" {
			facts.WokenAt = new(formatHubTime(operation.UpdatedAt))
		} else if operation.Attempts > 0 {
			facts.WakeFailedAt = new(formatHubTime(operation.UpdatedAt))
		}
		return nil
	})
	return facts, err
}

func (s *Service) spriteWakeRejected(ctx context.Context, op postgres.SpriteOperation) error {
	key, err := s.spriteLifecycleArchive(ctx, op.OrganizationID, spriteLifecycleReceipt{Log: "Runner service start was rejected"})
	if err != nil {
		return err
	}
	return s.saveSpriteOperation(ctx, &op, "pending", &key)
}

func (s *Service) monthlySpriteAllowedRecord(ctx context.Context, scope nativeScope) (bool, error) {
	exposure := budget.CostExposure{SpriteInfrastructure: true}
	decision, err := checkMonthlyBudget(ctx, nil, scope, exposure, false, s.config.now(), s.config.TenantRepository)
	if err != nil || decision.Allowed {
		return decision.Allowed, err
	}
	decision, err = checkMonthlyBudget(ctx, nil, scope, exposure, true, s.config.now(), s.config.TenantRepository)
	if err != nil || !decision.Allowed {
		return false, err
	}
	return false, ErrBillingBudgetUnavailable
}

func (s *Service) spritePoolSnapshotRecord(ctx context.Context, scope nativeScope, view spritePoolView) (spriteScaleInput, []spritePoolMember, error) {
	allocation, err := s.projectSpriteAllocation(ctx, scope)
	if err != nil {
		return spriteScaleInput{}, nil, err
	}
	if !allocation.Managed {
		scope.project = ""
	}
	placement := placementSnapshot{}
	if scope.project == "" {
		placement, err = s.spritePlacementRecord(ctx, scope)
		if err != nil {
			return spriteScaleInput{}, nil, err
		}
	}
	input := spriteScaleInput{Floor: view.MinRunners, Ceiling: view.MaxRunners, Depth: placement.SpriteTarget, Free: placement.SpriteFree, Pending: placement.Pending, CreateLimit: placement.ProviderFree}
	if placement.Policy.Mode == "local_first" {
		input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
	}
	if scope.project != "" {
		input.Floor = allocation.DesiredCount
		input.Ceiling = allocation.DesiredCount
		input.Depth, input.Free, input.Pending = 0, 0, 0
	}
	allowed, err := s.monthlySpriteAllowedRecord(ctx, scope)
	if err != nil {
		return input, nil, err
	}
	decision, err := checkMonthlyBudget(ctx, nil, scope, budget.CostExposure{SpriteInfrastructure: true}, false, s.config.now(), s.config.TenantRepository)
	if err != nil {
		return input, nil, err
	}
	if !decision.Allowed {
		input.CreateLimit = new(placement.Provisionable)
	}
	if !allowed {
		input.CreateLimit = new(0)
	}
	other := 0
	for _, member := range view.Members {
		if member.State != "deleted" && member.State != "deleting" && member.AllocationProjectID != scope.project {
			other++
		}
	}
	input.Ceiling = min(input.Ceiling, max(view.MaxRunners-other, 0))
	input.Floor = min(input.Floor, input.Ceiling)
	idle := []spritePoolMember{}
	retirement := map[string]int{}
	for _, member := range view.Members {
		if member.State == "deleted" || member.State == "deleting" || member.AllocationProjectID != scope.project {
			continue
		}
		input.Count++
		if member.State == "bootstrapping" {
			continue
		}
		if member.RunnerID == "" {
			if scope.project != "" {
				idle = append(idle, member)
			}
			continue
		}
		runner, err := s.readRunnerRouting(ctx, scope, member.RunnerID)
		if err != nil {
			return input, nil, err
		}
		busy, retained := false, false
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			var err error
			busy, err = owner.SpriteMemberBusy(ctx, member.Name)
			if err != nil {
				return err
			}
			enabled := false
			for _, project := range []tracker.ProjectID{"", scope.project} {
				policy, err := r.BillingMonthlyPolicy(ctx, project)
				if errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				enabled = enabled || policy.Policy.Enabled
			}
			if enabled {
				retained, err = owner.SpriteBudgetRetainsMember(ctx, member.Name)
			}
			return err
		})
		if err != nil {
			return input, nil, err
		}
		if busy || retained {
			if err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				owner, ok := w.(postgres.SpriteWriter)
				if !ok {
					return accessstore.ErrTransactionRequired
				}
				current, err := owner.SpriteMember(ctx, member.Name)
				if err != nil {
					return err
				}
				row := spriteMembershipRecord(current)
				row.IdleSince = s.config.now()
				return w.UpdateRunnerSpriteMembership(ctx, row)
			}); err != nil {
				return input, nil, err
			}
			if scope.project != "" {
				retirement[member.Name] = 2
				idle = append(idle, member)
			}
			continue
		}
		if scope.project != "" {
			ready := false
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				checks, err := r.RunnerLocalChecks(ctx, member.RunnerID, scope.project)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				ready = err == nil && checks.WorkReady()
				return err
			}); err != nil {
				return input, nil, err
			}
			if ready && runner.State == "active" && runner.ConnectionHealth == "online" {
				retirement[member.Name] = 1
			}
			idle = append(idle, member)
		} else if s.config.now().Sub(member.IdleSince) >= time.Duration(view.IdleSeconds)*time.Second {
			idle = append(idle, member)
		}
	}
	if scope.project != "" {
		slices.Reverse(idle)
		slices.SortStableFunc(idle, func(a, b spritePoolMember) int { return retirement[a.Name] - retirement[b.Name] })
	}
	input.Idle = len(idle)
	return input, idle, nil
}

func (s *Service) spriteOperationHTTP(ctx context.Context, op postgres.SpriteOperation, request spriteLifecycleRequest, method, path string, body []byte) (int, []byte, error) {
	token, err := s.spriteOperationToken(ctx, op, request)
	if err != nil {
		return 0, nil, err
	}
	defer clear(token)
	req, err := http.NewRequestWithContext(ctx, method, "https://api.sprites.dev/v1/sprites"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	defer req.Header.Del("Authorization")
	req.Header.Set("Content-Type", "application/json")
	response, err := s.spritePoolClient().Do(req)
	if err != nil {
		return 0, nil, errSpritesValidation
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, raw, err
}

func spriteOperationRetryAt(op postgres.SpriteOperation, now time.Time) time.Time {
	delay := time.Second * time.Duration(1<<min(op.Attempts, 8))
	digest := sha256.Sum256([]byte(string(op.OrganizationID) + "\x00" + op.IdempotencyKey))
	jitter := time.Duration(binary.BigEndian.Uint64(digest[:8])%31) * time.Millisecond
	return now.Add(delay + jitter)
}

func (s *Service) saveSpriteOperation(ctx context.Context, op *postgres.SpriteOperation, outcome string, receipt *string) error {
	next := *op
	next.Outcome = outcome
	next.ReceiptObjectKey = receipt
	next.UpdatedAt = s.config.now()
	next.NextAttemptAt = spriteOperationRetryAt(*op, next.UpdatedAt)
	err := s.config.TenantRepository.WriteTenant(ctx, op.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		return owner.UpdateSpriteOperation(ctx, next, op.Attempts)
	})
	if err == nil {
		next.Attempts++
		*op = next
	}
	return err
}

func (s *Service) completeSpriteOperation(ctx context.Context, op postgres.SpriteOperation, receipt spriteLifecycleReceipt, next string, request spriteLifecycleRequest) error {
	key, err := s.spriteLifecycleArchive(ctx, op.OrganizationID, receipt)
	if err != nil {
		return err
	}
	var retired *spritePoolMember
	err = s.config.TenantRepository.WriteTenant(ctx, op.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		op.Outcome = "committed"
		op.ReceiptObjectKey = &key
		if receipt.ResourceID != "" {
			op.ProviderResourceID = receipt.ResourceID
		}
		op.UpdatedAt = s.config.now()
		op.NextAttemptAt = op.UpdatedAt
		if err := owner.UpdateSpriteOperation(ctx, op, op.Attempts); err != nil {
			return err
		}
		if op.Operation == "wake" {
			return nil
		}
		if next != "" {
			_, err := owner.CreateSpriteOperation(ctx, postgres.SpriteOperation{OrganizationID: op.OrganizationID, IdempotencyKey: next + ":" + op.TargetEnrollmentID, Name: op.Name, Operation: next, Attempts: op.Attempts + 1, SourceRevision: op.SourceRevision, ProviderResourceID: op.ProviderResourceID, TargetEnrollmentID: op.TargetEnrollmentID, ProjectID: op.ProjectID, RequestObjectKey: op.RequestObjectKey, CreatedAt: op.UpdatedAt, NextAttemptAt: op.UpdatedAt})
			return err
		}
		member, err := owner.SpriteMember(ctx, op.Name)
		if err != nil {
			return err
		}
		row := spriteMembershipRecord(member)
		if op.Operation == "checkpoint" {
			wanted, err := spriteReservationWanted(ctx, w, op, request, s.config.now())
			if err != nil {
				return err
			}
			row.State = "enrolled"
			if !wanted {
				row.State = "deleting"
				retired = &spritePoolMember{Name: row.Name, EnrollmentID: row.EnrollmentID, AllocationProjectID: row.AllocationProjectID}
			}
			row.IdleSince = op.UpdatedAt
		}
		if op.Operation == "delete" {
			row.State = "deleted"
			id, err := owner.SpriteMemberRunner(ctx, op.Name)
			if err == nil {
				identity, err := w.RunnerIdentity(ctx, id)
				if err != nil {
					return err
				}
				if identity.RemovedAt == nil {
					if err := w.RemoveRunnerIdentity(ctx, id, op.UpdatedAt); err != nil {
						return err
					}
				}
				if err := w.RevokeAccessCredential(ctx, identity.TokenID, op.UpdatedAt); err != nil {
					return err
				}
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		return w.UpdateRunnerSpriteMembership(ctx, row)
	})
	if err == nil && retired != nil {
		return s.deletePoolSpriteRecord(ctx, nativeScope{organization: op.OrganizationID, project: op.ProjectID}, *retired)
	}
	return err
}

func spriteReservationWanted(ctx context.Context, r tenantstore.Reader, op postgres.SpriteOperation, request spriteLifecycleRequest, now time.Time) (bool, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return false, accessstore.ErrTransactionRequired
	}
	wanted, err := owner.SpriteMemberWanted(ctx, op.Name)
	if err != nil || !wanted {
		return false, err
	}
	pool, err := owner.SpritePool(ctx)
	if err != nil || pool.Revision != request.Settings.Revision {
		return false, err
	}
	if op.ProjectID != "" {
		member, err := owner.SpriteMember(ctx, op.Name)
		if err != nil {
			return false, err
		}
		allocation, err := owner.SpriteAllocation(ctx, op.ProjectID)
		if errors.Is(err, corestore.ErrNotFound) {
			return false, nil
		}
		if err != nil || allocation.Revision != member.AllocationRevision {
			return false, err
		}
	}
	enrollment, err := r.RunnerEnrollment(ctx, op.TargetEnrollmentID)
	if err != nil {
		return false, err
	}
	return enrollment.RevokedAt == nil && (enrollment.RedeemedAt != nil || enrollment.ExpiresAt.After(now)), nil
}

func spriteCreationLabel(op postgres.SpriteOperation) string {
	digest := sha256.Sum256([]byte("detent-sprite-create\x00" + string(op.OrganizationID) + "\x00" + op.IdempotencyKey))
	return fmt.Sprintf("detent-owner-%x", digest[:16])
}

func (s *Service) runSpriteOperation(ctx context.Context, op postgres.SpriteOperation) error {
	request, err := s.spriteLifecyclePayload(ctx, op)
	if err != nil {
		return err
	}
	if op.Outcome != "pending" && op.Outcome != "uncertain" {
		return nil
	}
	if op.Operation == "bootstrap" || op.Operation == "checkpoint" || op.Operation == "provision" && op.Outcome == "pending" {
		wanted := false
		err := s.config.TenantRepository.ReadTenant(ctx, op.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			wanted, err = spriteReservationWanted(ctx, r, op, request, s.config.now())
			return err
		})
		if err != nil {
			return err
		}
		if !wanted {
			return s.rejectSpriteOperation(ctx, op, "Sprite reservation is no longer current\n", op.Operation != "provision")
		}
	}
	if op.Outcome == "uncertain" {
		if err := s.saveSpriteOperation(ctx, &op, "uncertain", op.ReceiptObjectKey); err != nil {
			return err
		}
	}
	path := "/" + url.PathEscape(op.Name)
	if op.Operation == "bootstrap" || op.Operation == "checkpoint" {
		status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return spritesResponseError(status)
		}
		var resource struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &resource) != nil || resource.Name != op.Name || op.ProviderResourceID == "" || resource.ID != op.ProviderResourceID {
			return errSpritesValidation
		}
	}
	if op.Operation == "wake" {
		if op.Outcome == "uncertain" {
			status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodGet, path+"/services/detent-runner", nil)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return spritesResponseError(status)
			}
			var service struct {
				State *struct {
					Status string `json:"status"`
				} `json:"state"`
			}
			if json.Unmarshal(raw, &service) != nil {
				return errSpritesValidation
			}
			if service.State != nil && service.State.Status == "running" {
				return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{Log: "Runner service started"}, "", request)
			}
			return nil
		}
		if err := s.saveSpriteOperation(ctx, &op, "uncertain", nil); err != nil {
			return err
		}
		status, _, err := s.spriteOperationHTTP(ctx, op, request, http.MethodPost, path+"/services/detent-runner/start?duration=30s", nil)
		if err != nil {
			return ctx.Err()
		}
		if status < 200 || status > 299 {
			if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
				return s.spriteWakeRejected(ctx, op)
			}
			return nil
		}
		return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{Log: "Runner service started"}, "", request)
	}
	if op.Operation == "delete" {
		busy := false
		if err := s.config.TenantRepository.ReadTenant(ctx, op.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			var err error
			busy, err = owner.SpriteMemberBusy(ctx, op.Name)
			return err
		}); err != nil {
			return err
		}
		if busy {
			return nil
		}
		status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{ResourceID: op.ProviderResourceID}, "", request)
		}
		if status != http.StatusOK {
			return spritesResponseError(status)
		}
		var resource struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &resource) != nil || resource.Name != op.Name || resource.ID == "" || resource.ID != op.ProviderResourceID {
			return errSpritesValidation
		}
		if err := s.saveSpriteOperation(ctx, &op, "uncertain", nil); err != nil {
			return err
		}
		status, _, err = s.spriteOperationHTTP(ctx, op, request, http.MethodDelete, path, nil)
		if err != nil {
			return err
		}
		if status != http.StatusNotFound && (status < 200 || status > 299) {
			return spritesResponseError(status)
		}
		if status == http.StatusAccepted {
			return nil
		}
		return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{ResourceID: resource.ID}, "", request)
	}
	if op.Operation == "provision" {
		status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		switch status {
		case http.StatusOK:
			if op.Outcome == "pending" {
				return errors.Join(errSpriteNameTaken, s.rejectSpriteOperationWithRetry(ctx, op, "Sprite name is already in use\n", false, false))
			}
		case http.StatusNotFound:
			if err := s.saveSpriteOperation(ctx, &op, "uncertain", nil); err != nil {
				return err
			}
			body, err := json.Marshal(struct {
				Name   string   `json:"name"`
				Labels []string `json:"labels"`
			}{op.Name, []string{spriteCreationLabel(op)}})
			if err != nil {
				return fmt.Errorf("encode Sprite name: %w", err)
			}
			status, raw, err = s.spriteOperationHTTP(ctx, op, request, http.MethodPost, "", body)
			if err != nil {
				return err
			}
			if status != http.StatusCreated {
				if spriteNameCollision(status, raw) {
					return errors.Join(errSpriteNameTaken, s.rejectSpriteOperationWithRetry(ctx, op, "Sprite name is already in use\n", false, false))
				}
				cause := spritesResponseError(status)
				if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
					return errors.Join(cause, s.rejectSpriteOperation(ctx, op, cause.Error()+"\n", false))
				}
				return cause
			}
		default:
			return spritesResponseError(status)
		}
		var resource struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Labels []string `json:"labels"`
		}
		if json.Unmarshal(raw, &resource) != nil || resource.ID == "" || resource.Name != op.Name {
			return errSpritesValidation
		}
		if op.ProviderResourceID != "" && resource.ID != op.ProviderResourceID {
			return errSpritesValidation
		}
		if status == http.StatusOK && op.ProviderResourceID == "" && !slices.Contains(resource.Labels, spriteCreationLabel(op)) {
			return errSpritesValidation
		}
		return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{ResourceID: resource.ID}, "bootstrap", request)
	}
	if op.Operation == "bootstrap" {
		if err := s.saveSpriteOperation(ctx, &op, "uncertain", nil); err != nil {
			return err
		}
		log, err := s.execSpriteOperation(ctx, &op, request)
		if err != nil {
			if ctx.Err() != nil {
				return errors.Join(ctx.Err(), s.rejectSpriteOperation(ctx, op, "Bootstrap interrupted\n", true))
			}
			var exit spriteBootstrapExitError
			if errors.As(err, &exit) {
				log += fmt.Sprintf("Bootstrap exited with code %d\nBootstrap did not complete\n", exit.code)
				return errors.Join(err, s.rejectSpriteOperation(ctx, op, log, true))
			}
			return err
		}
		return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{Log: log}, "checkpoint", request)
	}
	if op.Operation == "checkpoint" {
		if op.Outcome == "uncertain" {
			status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodGet, path+"/checkpoints", nil)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return spritesResponseError(status)
			}
			var checkpoints []struct {
				Comment string `json:"comment"`
			}
			if json.Unmarshal(raw, &checkpoints) != nil {
				return errSpritesValidation
			}
			for _, checkpoint := range checkpoints {
				if checkpoint.Comment == op.IdempotencyKey {
					return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{Log: "Checkpoint completed\n"}, "", request)
				}
			}
			return nil
		}
		if err := s.saveSpriteOperation(ctx, &op, "uncertain", nil); err != nil {
			return err
		}
		body, err := json.Marshal(struct {
			Comment string `json:"comment"`
		}{op.IdempotencyKey})
		if err != nil {
			return fmt.Errorf("encode Sprite checkpoint: %w", err)
		}
		status, raw, err := s.spriteOperationHTTP(ctx, op, request, http.MethodPost, path+"/checkpoint", body)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return spritesResponseError(status)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		complete := false
		for {
			var event struct {
				Type string `json:"type"`
			}
			err := decoder.Decode(&event)
			if errors.Is(err, io.EOF) {
				break
			}
			if event.Type == "error" {
				return errors.Join(errSpritesValidation, s.rejectSpriteOperation(ctx, op, "Checkpoint did not complete\n", true))
			}
			if err != nil {
				return errSpritesValidation
			}
			complete = complete || event.Type == "complete"
		}
		if !complete {
			return errSpritesValidation
		}
		return s.completeSpriteOperation(ctx, op, spriteLifecycleReceipt{Log: "Checkpoint completed\n"}, "", request)
	}
	return nil
}

func (s *Service) deletePoolSpriteRecord(ctx context.Context, scope nativeScope, member spritePoolMember) error {
	return s.retirePoolSpriteRecord(ctx, scope, member, true)
}

func (s *Service) retirePoolSpriteRecord(ctx context.Context, scope nativeScope, member spritePoolMember, execute bool) error {
	var operation postgres.SpriteOperation
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		pool, err := owner.LockSpritePool(ctx)
		if err != nil {
			return err
		}
		if _, err := s.spriteLifecycleAuthority(ctx, w, scope.organization, pool.ConfiguredBy); err != nil {
			return err
		}
		current, err := owner.SpriteMember(ctx, member.Name)
		if err != nil {
			return err
		}
		if current.EnrollmentID != member.EnrollmentID || tracker.ProjectID(current.AllocationProjectID) != scope.project {
			return corestore.ErrNotFound
		}
		if current.State == "deleted" {
			return nil
		}
		if current.State == "enrolled" {
			count, err := owner.SpriteRetainedMemberCount(ctx, scope.project)
			if err != nil {
				return err
			}
			total, err := owner.SpriteRetainedPoolCount(ctx)
			if err != nil {
				return err
			}
			floor := int64(pool.MinRunners)
			if scope.project != "" {
				allocation, err := owner.SpriteAllocation(ctx, scope.project)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				floor = int64(allocation.DesiredCount)
			}
			floor = min(floor, max(int64(pool.MaxRunners)-(total-count), 0))
			if count <= floor {
				return nil
			}
			if scope.project == "" && pool.MaxRunners > 0 {
				busy, err := owner.SpriteMemberBusy(ctx, current.Name)
				if err != nil {
					return err
				}
				if busy || s.config.now().Sub(current.IdleSince) < time.Duration(pool.IdleSeconds)*time.Second {
					return nil
				}
			}
		}
		provision, err := owner.SpriteOperation(ctx, "provision:"+member.EnrollmentID)
		if err != nil {
			return err
		}
		if provision.Outcome != "committed" {
			return nil
		}
		row := spriteMembershipRecord(current)
		row.State = "deleting"
		if err := w.UpdateRunnerSpriteMembership(ctx, row); err != nil {
			return err
		}
		enrollment, err := w.RunnerEnrollment(ctx, member.EnrollmentID)
		if err != nil {
			return err
		}
		if current.State == "bootstrapping" && current.AllocationProjectID != "" && !enrollment.ExpiresAt.After(s.config.now()) {
			allocation, err := owner.SpriteAllocation(ctx, scope.project)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil && allocation.Revision == current.AllocationRevision {
				if err := owner.RecordSpriteProvisionFailure(ctx, scope.project, current.AllocationRevision); err != nil {
					return err
				}
			}
		}
		if enrollment.RedeemedAt == nil && enrollment.RevokedAt == nil {
			if err := w.RevokeRunnerEnrollment(ctx, member.EnrollmentID, s.config.now()); err != nil {
				return err
			}
		}
		id, err := owner.SpriteMemberRunner(ctx, member.Name)
		if err == nil {
			identity, err := w.RunnerIdentity(ctx, id)
			if err != nil {
				return err
			}
			identity.State = "draining"
			if err := w.UpdateRunnerRouting(ctx, identity.Revision, identity); err != nil {
				return err
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		now := s.config.now()
		operation, err = owner.CreateSpriteOperation(ctx, postgres.SpriteOperation{OrganizationID: scope.organization, IdempotencyKey: "delete:" + member.EnrollmentID, Name: member.Name, Operation: "delete", TargetEnrollmentID: member.EnrollmentID, ProjectID: scope.project, RequestObjectKey: provision.RequestObjectKey, ProviderResourceID: provision.ProviderResourceID, CreatedAt: now, NextAttemptAt: now})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil || operation.IdempotencyKey == "" {
		return err
	}
	if !execute {
		return nil
	}
	return s.runSpriteOperation(ctx, operation)
}

func (s *Service) scaleSpritePoolRecord(ctx context.Context, scope nativeScope, allowCreate bool) error {
	var pending []postgres.SpriteOperation
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		var err error
		pending, err = owner.PendingSpriteOperations(ctx, s.config.now(), 20)
		return err
	})
	if err != nil {
		return err
	}
	var operationErrors error
	for _, operation := range pending {
		if err := s.runSpriteOperation(ctx, operation); err != nil {
			operationErrors = errors.Join(operationErrors, err)
		}
	}
	scope, err = s.spritePoolLifecycleScope(ctx, scope.organization)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, accessstore.ErrDenied) {
		return operationErrors
	}
	if err != nil {
		return err
	}
	view, err := s.readSpritePoolFactsRecord(ctx, scope)
	if err != nil {
		return err
	}
	expired := 0
	for _, member := range view.Members {
		if member.State != "bootstrapping" || expired == 20 {
			continue
		}
		var enrollment runnerstore.Enrollment
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			enrollment, err = r.RunnerEnrollment(ctx, member.EnrollmentID)
			return err
		})
		if err != nil {
			return errors.Join(operationErrors, err)
		}
		if !enrollment.ExpiresAt.After(s.config.now()) {
			ownerScope := scope
			ownerScope.project = member.AllocationProjectID
			if err := s.deletePoolSpriteRecord(ctx, ownerScope, member); err != nil {
				return errors.Join(operationErrors, err)
			}
			expired++
		}
	}
	if expired > 0 {
		view, err = s.readSpritePoolFactsRecord(ctx, scope)
		if err != nil {
			return errors.Join(operationErrors, err)
		}
	}
	projects := []tracker.ProjectID{""}
	seen := map[tracker.ProjectID]bool{"": true}
	names := make([]string, 0, len(view.Members))
	for _, member := range view.Members {
		if member.State != "deleted" && member.AllocationProjectID != "" {
			names = append(names, member.Name)
		}
	}
	if len(names) > 0 {
		var ordered []tracker.ProjectID
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			var err error
			ordered, err = owner.SpriteRetirementProjects(ctx, names)
			return err
		})
		if err != nil {
			return errors.Join(operationErrors, err)
		}
		for _, project := range ordered {
			seen[project] = true
			projects = append(projects, project)
		}
	}
	for _, member := range view.Members {
		if member.State != "deleted" && !seen[member.AllocationProjectID] {
			seen[member.AllocationProjectID] = true
			projects = append(projects, member.AllocationProjectID)
		}
	}
	retired := 0
	for _, project := range projects {
		ownerScope := scope
		ownerScope.project = project
		input, idle, err := s.spritePoolSnapshotRecord(ctx, ownerScope, view)
		if err != nil {
			return errors.Join(operationErrors, err)
		}
		decision := decideSpriteScale(input)
		for _, member := range idle[:min(decision.Delete, 20-retired)] {
			if err := s.deletePoolSpriteRecord(ctx, ownerScope, member); err != nil {
				return errors.Join(operationErrors, err)
			}
			retired++
		}
		if decision.Delete > 0 {
			view, err = s.readSpritePoolFactsRecord(ctx, scope)
			if err != nil {
				return errors.Join(operationErrors, err)
			}
		}
		if retired == 20 {
			break
		}
	}
	for range 20 {
		created, err := s.createPoolSprite(ctx, scope, view.spritePoolSettings, allowCreate)
		if err != nil {
			return err
		}
		if !created {
			break
		}
	}
	return operationErrors
}

func (s *Service) wakeSpriteRunnersRecord(ctx context.Context, scope nativeScope, state string) (int, error) {
	allowed, err := s.monthlySpriteAllowedRecord(ctx, scope)
	if err != nil || !allowed {
		return 0, err
	}
	project, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return 0, err
	}
	if project.Core.DeletedAt != nil {
		return 0, corestore.ErrNotFound
	}
	dispatchable := false
	for _, candidate := range project.States {
		if candidate.Name == state && candidate.Dispatchable && !candidate.Terminal {
			dispatchable = true
		}
	}
	if !dispatchable {
		return 0, nil
	}
	placement, err := s.spriteProjectPlacementRecord(ctx, scope)
	if err != nil {
		return 0, err
	}
	if placement.poolConfigured && placement.Provisionable == 0 {
		return 0, nil
	}
	total := 0
	slices.SortFunc(placement.Runners, func(a, b placementRunner) int {
		if order := strings.Compare(a.Hostname, b.Hostname); order != 0 {
			return order
		}
		return strings.Compare(a.RunnerID, b.RunnerID)
	})
	seen := map[string]bool{}
	for _, candidate := range placement.Runners {
		if total >= 20 {
			break
		}
		if !candidate.Sprite || !candidate.Paused || candidate.State != "active" || !validSpritesSlug(candidate.Hostname) || seen[candidate.Hostname] {
			continue
		}
		allowed := false
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			identity, err := r.RunnerIdentity(ctx, candidate.RunnerID)
			if err != nil {
				return err
			}
			if !identity.LastHeartbeatAt.Equal(candidate.LastHeartbeatAt) || s.config.now().Sub(identity.LastHeartbeatAt) < spriteRunnerIdle {
				return nil
			}
			project, err := r.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			dispatchable := slices.ContainsFunc(project.States, func(current tracker.NativeState) bool {
				return current.Name == state && current.Dispatchable && !current.Terminal
			})
			if !dispatchable || project.Core.DeletedAt != nil {
				return nil
			}
			allowed, err = owner.SpriteRunnerAllowed(ctx, candidate.RunnerID, scope.project, s.config.now())
			return err
		})
		if err != nil {
			return total, err
		}
		if !allowed {
			continue
		}
		secretScope, provider, _, err := s.spriteWakeSecretRecord(ctx, scope, candidate.Hostname)
		if errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return total, err
		}
		key, err := s.spriteLifecycleArchive(ctx, scope.organization, spriteLifecycleRequest{Actor: scope.credential.ID, TokenProject: secretScope.project, Provider: provider, State: state})
		if err != nil {
			return total, err
		}
		var op postgres.SpriteOperation
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			owner, ok := w.(postgres.SpriteWriter)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			now := s.config.now()
			var err error
			op, err = owner.CreateSpriteOperation(ctx, postgres.SpriteOperation{OrganizationID: scope.organization, IdempotencyKey: "wake:" + candidate.RunnerID + ":" + candidate.LastHeartbeatAt.UTC().Format(time.RFC3339Nano), Name: candidate.Hostname, Operation: "wake", TargetEnrollmentID: candidate.Enrollment, ProjectID: scope.project, RequestObjectKey: &key, CreatedAt: now, NextAttemptAt: now})
			return err
		})
		if err != nil {
			return total, err
		}
		seen[candidate.Hostname] = true
		if op.Outcome == "committed" || op.NextAttemptAt.After(s.config.now()) {
			continue
		}
		if err := s.runSpriteOperation(ctx, op); err != nil {
			return total, err
		}
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			current, err := owner.SpriteOperation(ctx, op.IdempotencyKey)
			if err == nil && current.Outcome == "committed" {
				total++
			}
			return err
		}); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) wakeOrganizationSpriteRunnersRecord(ctx context.Context, scope nativeScope) (int, error) {
	var projects []corestore.Project
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if scope.project != "" {
			project, err := r.ProjectRecord(ctx, scope.project)
			if err == nil {
				projects = []corestore.Project{project.Core}
			}
			return err
		}
		var err error
		projects, err = r.Projects(ctx, "", 100)
		return err
	})
	if err != nil {
		return 0, err
	}
	if scope.credential.ID == "" {
		scope, err = s.spritePoolLifecycleScope(ctx, scope.organization)
		if errors.Is(err, corestore.ErrNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
	}
	total := 0
	for _, project := range projects {
		if project.DeletedAt != nil || project.Profile != "native" {
			continue
		}
		scope.project = project.ID
		record, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return total, err
		}
		for _, state := range record.States {
			if !state.Dispatchable || state.Terminal {
				continue
			}
			count, err := s.wakeSpriteRunnersRecord(ctx, scope, state.Name)
			if err != nil {
				return total, err
			}
			total += count
			if total >= 20 {
				return total, nil
			}
		}
	}
	return total, nil
}

func (s *Service) execSpriteOperation(ctx context.Context, op *postgres.SpriteOperation, request spriteLifecycleRequest) (string, error) {
	token, err := s.spriteOperationToken(ctx, *op, request)
	if err != nil {
		return "", err
	}
	defer clear(token)
	query := url.Values{"cmd": {"bash", "-s"}, "stdin": {"true"}, "max_run_after_disconnect": {"1s"}}
	path := "/exec?" + query.Encode()
	if op.ExecSessionID != nil {
		path = "/exec/" + url.PathEscape(*op.ExecSessionID)
	}
	header := http.Header{"Authorization": {"Bearer " + string(token)}}
	defer header.Del("Authorization")
	conn, response, err := websocket.Dial(ctx, "wss://api.sprites.dev/v1/sprites/"+url.PathEscape(op.Name)+path, &websocket.DialOptions{HTTPClient: s.spritePoolClient(), HTTPHeader: header})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return "", errSpritesValidation
	}
	defer func() {
		if err := conn.CloseNow(); err != nil {
			s.config.Logger.Debug("close Sprite bootstrap connection", "error", err)
		}
	}()
	conn.SetReadLimit(1 << 20)
	if op.ExecSessionID == nil {
		kind, data, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageText {
			return "", errSpritesValidation
		}
		var session struct {
			Type string `json:"type"`
			ID   string `json:"session_id"`
		}
		if json.Unmarshal(data, &session) != nil || session.Type != "session_info" || session.ID == "" {
			return "", errSpritesValidation
		}
		if err := s.config.TenantRepository.WriteTenant(ctx, op.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(postgres.SpriteWriter)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			return owner.SetSpriteExecSession(ctx, *op, session.ID)
		}); err != nil {
			return "", err
		}
		op.ExecSessionID = &session.ID
		plain, err := s.config.SecretKeys.Open(request.EnrollmentToken, secretAAD(string(op.OrganizationID), op.TargetEnrollmentID, "sprite-enrollment"))
		if err != nil {
			return "", err
		}
		defer clear(plain)
		command := "detent hub runner register --url " + spriteShellQuote(s.config.Hosted.PublicURL) + " --organization " + spriteShellQuote(string(op.OrganizationID)) + " --name " + spriteShellQuote(op.Name) + " --capacity 1 --isolation-tier " + spriteShellQuote(request.Settings.IsolationTier) + " --token " + spriteShellQuote(string(plain))
		script := []byte(spritePoolBootstrapScript(op.OrganizationID, s.config.Hosted.PublicURL, request.Settings.Bootstrap, command, request.Version))
		defer clear(script)
		input := append([]byte{0}, script...)
		defer clear(input)
		if err := conn.Write(ctx, websocket.MessageBinary, input); err != nil {
			return "", errSpritesValidation
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte{4}); err != nil {
			return "", errSpritesValidation
		}
	}
	var log strings.Builder
	pending := ""
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return log.String(), errSpritesValidation
		}
		if kind == websocket.MessageBinary && len(data) == 2 && data[0] == 3 {
			if data[1] != 0 {
				return log.String(), spriteBootstrapExitError{code: int(data[1])}
			}
			return log.String(), nil
		}
		if kind == websocket.MessageBinary && len(data) > 1 && data[0] == 1 {
			pending += string(data[1:])
			for {
				line, rest, found := strings.Cut(pending, "\n")
				if !found {
					if len(pending) > 8192 {
						pending = ""
					}
					break
				}
				pending = rest
				if safe := spriteBootstrapDiagnostic(line); safe != "" && log.Len()+len(safe) < 8192 {
					log.WriteString(safe + "\n")
				}
			}
		}
		if kind == websocket.MessageText {
			var event struct {
				Type     string `json:"type"`
				ExitCode *int   `json:"exit_code"`
			}
			if json.Unmarshal(data, &event) == nil && event.Type == "exit" && event.ExitCode != nil {
				if *event.ExitCode != 0 {
					return log.String(), spriteBootstrapExitError{code: *event.ExitCode}
				}
				return log.String(), nil
			}
		}
	}
}
