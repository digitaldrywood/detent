package runnerrepos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (h *httpRoutes) request(c echo.Context) error {
	var request struct {
		tracker.Mutation
		RunnerID string `json:"runner_id"`
	}
	if err := h.deps.Decode(c, &request); err != nil {
		return h.deps.Invalid(c, err)
	}
	scope := h.deps.Input(c)
	scope.Admin = true
	requestID := hubapi.NewID("repos")
	return h.mutate(c, scope, request.Mutation, request, map[string]string{"request_id": requestID}, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		identity, err := w.RunnerIdentity(ctx, request.RunnerID)
		if err != nil || identity.OrganizationID != scope.Organization || identity.RemovedAt != nil {
			return hubapi.NotFound()
		}
		return w.RequestRunnerRepositories(ctx, request.RunnerID, requestID)
	})
}

func (h *httpRoutes) report(c echo.Context) error {
	var request struct {
		tracker.Mutation
		runnerauth.RepositoryList
	}
	if err := h.deps.Decode(c, &request); err != nil {
		return h.deps.Invalid(c, err)
	}
	if err := request.Validate(); err != nil {
		return h.deps.Invalid(c, err)
	}
	scope := h.deps.Input(c)
	return h.mutate(c, scope, request.Mutation, request, map[string]bool{"saved": true}, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		identity, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.NotFound()
			}
			return err
		}
		credential, err := w.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.NotFound()
			}
			return err
		}
		if identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.MachineID != scope.Credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(now) || credential.OrganizationID != scope.Organization || credential.Scope != string(hubauth.Worker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubapi.NotFound()
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.NotFound()
		}
		list, err := w.RunnerRepositoryList(ctx, identity.RunnerID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.PublicInvalid("Repository list request is no longer current")
			}
			return err
		}
		if !list.Pending || list.RequestID != request.RequestID {
			return hubapi.PublicInvalid("Repository list request is no longer current")
		}
		listedAt := now
		list.Reason = request.Reason
		list.Pending = false
		list.ListedAt = &listedAt
		repositories := make([]runnerstore.Repository, 0, len(request.Repositories))
		for _, repository := range request.Repositories {
			repositories = append(repositories, runnerstore.Repository{OrganizationID: scope.Organization, RunnerID: identity.RunnerID, Owner: repository.Owner, Name: repository.Name, Private: repository.Private, PushedAt: repository.PushedAt, ListedAt: listedAt})
		}
		if err := w.ReplaceRunnerRepositories(ctx, list, repositories); err != nil {
			if errors.Is(err, corestore.ErrRevisionConflict) {
				return hubapi.PublicInvalid("Repository list request is no longer current")
			}
			return err
		}
		return nil
	})
}

func (h *httpRoutes) mutate(c echo.Context, scope hubauth.Input, command tracker.Mutation, input, response any, mutate func(context.Context, tenantstore.Writer, time.Time) error) error {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return h.deps.Error(c, hubapi.PublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	s := h.deps.Service()
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return h.deps.Error(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	content := eventstore.NewContent(s.deps.Details())
	responseRaw, err := json.Marshal(response)
	if err != nil {
		return h.deps.Error(c, err)
	}
	preparedAt := s.deps.Now().UTC()
	preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: preparedAt}, responseRaw)
	if err != nil {
		return h.deps.Error(c, err)
	}
	usageHash, err := hubapi.CommandHash([]string{string(scope.Organization), scope.Credential.ID, operation, command.IdempotencyKey})
	if err != nil {
		return h.deps.Error(c, err)
	}
	var prior *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := s.deps.Now().UTC()
		if scope.Credential.Runner.RunnerID != "" {
			if _, err := authorizeRunnerRepositoryWriter(ctx, w, scope, now); err != nil {
				return err
			}
		} else if err := s.deps.Auth().Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		receipt, err := w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return hubapi.Conflict(0)
			}
			prior = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.deps.RequireFeature(ctx, w, h.deps.Feature(c), now); err != nil {
			return err
		}
		if err := mutate(ctx, w, now); err != nil {
			return err
		}
		if err := s.deps.ApplyUsage(ctx, w, now, "runner_"+usageHash, false, false); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, preparedReceipt)
		return err
	})
	if err == nil {
		if prior != nil {
			response, readErr := content.Receipt(ctx, *prior)
			if readErr != nil {
				return h.deps.Error(c, readErr)
			}
			return c.JSONBlob(http.StatusOK, response)
		}
		return c.JSONBlob(http.StatusOK, responseRaw)
	}
	if errors.Is(err, corestore.ErrUniqueViolation) || errors.Is(err, corestore.ErrSerializationFailure) {
		var replay *eventstore.Receipt
		readErr := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			now := s.deps.Now().UTC()
			if scope.Credential.Runner.RunnerID != "" {
				if _, err := authorizeRunnerRepositoryWriter(ctx, w, scope, now); err != nil {
					return err
				}
			} else if err := s.deps.Auth().Authorize(ctx, w, scope, true); err != nil {
				return err
			}
			receipt, err := w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if receipt.RequestHash != hash {
				return hubapi.Conflict(0)
			}
			replay = &receipt
			return nil
		})
		if readErr != nil {
			return h.deps.Error(c, readErr)
		}
		if replay != nil {
			response, readErr := content.Receipt(ctx, *replay)
			if readErr != nil {
				return h.deps.Error(c, readErr)
			}
			return c.JSONBlob(http.StatusOK, response)
		}
	}
	return h.deps.Error(c, s.deps.RecordError(err))
}

func (s *Service) AuthorizeProject(ctx context.Context, scope hubauth.Input) error {
	return s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := authorizeRunnerRepositoryReader(ctx, r, scope, s.deps.Now().UTC())
		return err
	})
}

func authorizeRunnerRepositoryWriter(ctx context.Context, w tenantstore.Writer, scope hubauth.Input, now time.Time) (runnerstore.Identity, error) {
	return authorizeRunnerRepositoryReader(ctx, w, scope, now)
}

func authorizeRunnerRepositoryReader(ctx context.Context, r tenantstore.Reader, scope hubauth.Input, now time.Time) (runnerstore.Identity, error) {
	identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return runnerstore.Identity{}, hubapi.NotFound()
		}
		return runnerstore.Identity{}, err
	}
	credential, err := r.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return runnerstore.Identity{}, hubapi.NotFound()
		}
		return runnerstore.Identity{}, err
	}
	if identity.OrganizationID != scope.Organization || identity.RunnerID != scope.Credential.Runner.RunnerID || identity.MachineID != scope.Credential.Runner.MachineID || identity.RemovedAt != nil || identity.TokenID != credential.ID || !identity.ExpiresAt.After(now) || credential.OrganizationID != scope.Organization || credential.Scope != string(hubauth.Worker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
		return runnerstore.Identity{}, hubapi.NotFound()
	}
	allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return runnerstore.Identity{}, err
	}
	if !allowed {
		return runnerstore.Identity{}, hubapi.NotFound()
	}
	return identity, nil
}
