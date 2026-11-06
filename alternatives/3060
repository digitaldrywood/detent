package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) redeemRunnerEnrollmentTenant(ctx context.Context, organization tracker.OrganizationID, token string, request runnerauth.Redemption) (runnerauth.Identity, error) {
	var result runnerauth.Identity
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := s.config.now().UTC()
		enrollment, err := w.RunnerEnrollmentByTokenHash(ctx, apikey.HashToken(token))
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return runnerUnauthorized()
			}
			return err
		}
		bound := runnerauth.Binding{RunnerID: enrollment.RunnerID, MachineID: enrollment.MachineID}
		unbound := bound == (runnerauth.Binding{})
		if enrollment.OrganizationID != organization || !unbound && bound != request.Binding || enrollment.RedeemedAt != nil || enrollment.RevokedAt != nil || !now.Before(enrollment.ExpiresAt) || now.Before(enrollment.CreatedAt) || !runnerauth.ValidOperations(enrollment.Operations) {
			return runnerUnauthorized()
		}
		binding := request.Binding
		if err := w.RunnerBindingAvailable(ctx, binding.RunnerID, binding.MachineID, enrollment.SharedMachine); err != nil {
			if errors.Is(err, runnerstore.ErrIdentityCollision) {
				return runnerCollision()
			}
			return err
		}
		if request.SpriteName != "" {
			machine, err := w.Machine(ctx, string(binding.MachineID))
			if enrollment.SharedMachine {
				if err != nil {
					return err
				}
				if machine.Hostname != request.SpriteName {
					return nativePublicInvalid("Sprite name must match the runner hostname")
				}
			}
		}
		expiresAt := now.Add(runnerauth.CredentialTTL)
		hash := apikey.HashToken(request.Credential)
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: binding.RunnerID, Name: binding.RunnerID, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: string(apiScopeWorker), NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expiresAt, ProjectAccess: "selected", Kind: "service"}); err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return runnerCollision()
			}
			return err
		}
		if !enrollment.SharedMachine {
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: string(binding.MachineID), Hostname: request.Hostname, DisplayName: request.DisplayName, Capacity: int64(request.Capacity), Version: request.Version, SpriteName: request.SpriteName}); err != nil {
				if errors.Is(err, corestore.ErrUniqueViolation) {
					return runnerCollision()
				}
				return err
			}
		}
		for _, projectID := range enrollment.ProjectIDs {
			project, err := w.ProjectRecord(ctx, projectID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return runnerUnauthorized()
				}
				return err
			}
			if project.Core.DeletedAt != nil {
				return runnerUnauthorized()
			}
			if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: binding.RunnerID, ProjectID: projectID}); err != nil {
				return err
			}
		}
		tags := []string{}
		if request.SpriteName != "" {
			member, err := w.RunnerSpriteMembershipByEnrollment(ctx, enrollment.ID)
			if err == nil && member.OrganizationID == organization && member.Name == request.SpriteName && member.State != "deleted" {
				tags = []string{"sprite"}
			} else if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		identity := runnerstore.Identity{OrganizationID: organization, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: binding.RunnerID, EnrollmentID: enrollment.ID, Scope: enrollment.Scope, Operations: append([]string(nil), enrollment.Operations...), DisplayName: request.DisplayName, CapacityLimit: int64(request.Capacity), ReportedCapacity: int64(request.Capacity), OS: request.OS, Architecture: request.Architecture, Tags: tags, IsolationTier: request.IsolationTier, CreatedAt: now, ExpiresAt: expiresAt, LastHeartbeatAt: now}
		if err := w.RedeemRunnerEnrollment(ctx, enrollment.ID, binding.RunnerID, binding.MachineID, now); err != nil {
			if errors.Is(err, corestore.ErrRevisionConflict) {
				return runnerUnauthorized()
			}
			return err
		}
		if err := w.CreateRunnerIdentity(ctx, identity); err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return runnerCollision()
			}
			return err
		}
		if err := w.ReplaceRunnerBackendIsolation(ctx, binding.RunnerID, request.BackendIsolation); err != nil {
			return err
		}
		if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: binding.RunnerID, ActorID: enrollment.CreatedBy, Kind: "enrolled", OccurredAt: now}); err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, w, now, newNativeID("runner_redemption"), false, false); err != nil {
			return err
		}
		result = runnerauth.Identity{Binding: binding, Scope: enrollment.Scope, OrganizationID: organization, ProjectIDs: append([]tracker.ProjectID(nil), enrollment.ProjectIDs...), Operations: append([]string(nil), enrollment.Operations...), ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		return runnerauth.Identity{}, err
	}
	return result, nil
}
