package runnerenroll

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func Unauthorized() error {
	return &hubapi.Error{Code: "unauthorized", Message: "Runner credential or enrollment is invalid or inactive", Status: http.StatusUnauthorized}
}

func Collision() error {
	return &hubapi.Error{Code: "identity_collision", Message: "Identity is already registered; create a fresh host identity and enroll it explicitly", Status: http.StatusConflict, Public: true}
}

func (s *Service) Create(ctx context.Context, scope hubapi.Scope, request runnerauth.EnrollmentRequest) (any, error) {
	if !request.Valid() && !request.Unbound() || !runnerauth.ValidOperations(request.Operations) || request.Scope != "" && request.Scope != "projects" && request.Scope != "organization" || request.Scope == "organization" && len(request.ProjectIDs) > 0 || request.Scope != "organization" && len(request.ProjectIDs) == 0 || len(request.ProjectIDs) > 100 || request.TTLSeconds <= 0 || request.TTLSeconds > int64(runnerauth.MaxEnrollmentTTL/time.Second) {
		return nil, &hubapi.Error{Code: "invalid_request", Message: "Enrollment requires valid or omitted host IDs, a project or organization scope and operations, and a TTL of 1 to 900 seconds", Status: http.StatusUnprocessableEntity, Public: true}
	}
	if request.Scope == "" {
		request.Scope = "projects"
	}
	token, err := s.deps.GenerateToken()
	if err != nil {
		return nil, err
	}
	var enrollment runnerauth.Enrollment
	err = s.Write(ctx, scope, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		for i, project := range request.ProjectIDs {
			_, err := w.ProjectVersion(ctx, project)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.PublicInvalid("Enrollment projects must be unique and belong to the organization")
				}
				return err
			}
			if slices.Contains(request.ProjectIDs[:i], project) {
				return hubapi.PublicInvalid("Enrollment projects must be unique and belong to the organization")
			}
		}
		if !request.Unbound() {
			if err := w.RunnerBindingAvailable(ctx, request.RunnerID, request.MachineID, request.SharedMachine); err != nil {
				if errors.Is(err, runnerstore.ErrIdentityCollision) {
					return Collision()
				}
				return err
			}
		}
		enrollment = runnerauth.Enrollment{ID: s.deps.NewID("enrollment"), Token: token, ExpiresAt: now.Add(time.Duration(request.TTLSeconds) * time.Second)}
		if err := w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: scope.Organization, ID: enrollment.ID, RunnerID: request.RunnerID, MachineID: request.MachineID, TokenHash: apikey.HashToken(token), Scope: request.Scope, SharedMachine: request.SharedMachine, Operations: slices.Clone(request.Operations), ProjectIDs: slices.Clone(request.ProjectIDs), CreatedAt: now, ExpiresAt: enrollment.ExpiresAt, CreatedBy: scope.CredentialID}); err != nil {
			return err
		}
		return s.deps.ApplyUsage(ctx, w, now, enrollment.ID, false, false)
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

func (s *Service) Revoke(ctx context.Context, scope hubapi.Scope, resource string) (any, error) {
	if err := s.Write(ctx, scope, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		if err := w.RevokeRunnerEnrollment(ctx, resource, now); errors.Is(err, corestore.ErrRevisionConflict) {
			return hubapi.NotFound()
		} else {
			if err != nil {
				return err
			}
		}
		return s.deps.ApplyUsage(ctx, w, now, resource, true, false)
	}); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Service) Write(ctx context.Context, scope hubapi.Scope, write func(context.Context, tenantstore.Writer, time.Time) error) error {
	if scope.Organization == "" {
		return hubapi.NotFound()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		return write(ctx, w, s.deps.Now().UTC())
	})
}
