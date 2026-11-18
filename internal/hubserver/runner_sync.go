package hubserver

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type syncVersionReader interface {
	RunnerSyncVersions(context.Context, runnerstore.Identity) (map[tracker.ProjectID]runnerauth.SyncProjectVersion, error)
}

func (s *Service) syncRunner(c echo.Context) error {
	var request runnerauth.SyncRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	org := tracker.OrganizationID(c.Param("organization"))
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || credential.Runner.OrganizationID != org || credential.Runner.MachineID != request.MachineID {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if request.WaitSeconds < 0 || request.WaitSeconds > 30 || len(request.Versions.Projects) > runnerstore.MaxRunnerProjectList {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid runner sync bounds"))
	}
	key := dispatchNotificationKey(org)
	cursor := s.notifications.dispatchCursor(key)
	if request.WaitSeconds > 0 && request.Versions.Cursor == cursor {
		subscription, release := s.notifications.subscribe(key)
		defer release()
		if cursor == s.notifications.dispatchCursor(key) {
			ctx, cancel := context.WithTimeout(c.Request().Context(), time.Duration(request.WaitSeconds)*time.Second)
			select {
			case <-ctx.Done():
			case <-subscription.closed:
				cancel()
				return c.NoContent(http.StatusServiceUnavailable)
			case <-subscription.wake:
			}
			cancel()
		}
		cursor = s.notifications.dispatchCursor(key)
	}
	ctx, cancel := s.requestTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	result, err := s.readRunnerSync(ctx, hubscope.NativeScope{Organization: org, Credential: credential}, request, cursor)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readRunnerSync(ctx context.Context, scope hubscope.NativeScope, request runnerauth.SyncRequest, cursor string) (runnerauth.SyncResponse, error) {
	var result runnerauth.SyncResponse
	var versions runnerauth.SyncVersions
	configs := map[tracker.ProjectID]postgres.ConfigurationProject{}
	policies := map[tracker.ProjectID]policystore.Revision{}
	repo, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return result, hubapi.NativeRunEventStorageUnavailable()
	}
	err := repo.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		token, err := r.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		now := s.config.Clock().UTC()
		identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if token.OrganizationID != scope.Organization || token.Scope != string(hubapi.APIScopeWorker) || token.Kind != "service" || !token.Valid(scope.Credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, token.ID, request.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Read) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubscope.RunnerUnauthorized()
		}
		reader, ok := r.(syncVersionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		projects, err := reader.RunnerSyncVersions(ctx, identity)
		if err != nil {
			return err
		}
		versions = runnerauth.SyncVersions{Routing: identity.Revision, Cursor: cursor, Projects: projects}
		if versions.Routing != request.Versions.Routing || !sameSyncProjects(projects, request.Versions.Projects) {
			result.Routing = new(runnerauth.RoutingSnapshot)
			if err := readRunnerResponseRouting(ctx, r, identity, result.Routing); err != nil {
				return err
			}
		}
		for project, v := range projects {
			prev, seen := request.Versions.Projects[project]
			if seen && prev == v {
				continue
			}
			if result.Projects == nil {
				result.Projects = map[tracker.ProjectID]runnerauth.SyncProject{}
			}
			result.Projects[project] = runnerauth.SyncProject{}
			if v.Policy != "" && (!seen || v.Policy != prev.Policy) {
				row, err := r.ProjectPolicy(ctx, project)
				if err != nil {
					return err
				}
				policies[project] = row
			}
			if v.Configuration != "" && (!seen || v.Configuration != prev.Configuration) {
				configReader, ok := r.(postgres.ConfigurationReader)
				if !ok {
					return hubapi.NativeRunEventStorageUnavailable()
				}
				row, err := configReader.ConfigurationProject(ctx, identity.RunnerID, project)
				if err != nil {
					return err
				}
				configs[project] = row
			}
		}
		return nil
	})
	if err != nil {
		return runnerauth.SyncResponse{}, err
	}
	for project, row := range policies {
		approval, err := s.decodeTenantProjectPolicy(ctx, scope.Organization, project, row)
		if err != nil {
			return runnerauth.SyncResponse{}, err
		}
		delta := result.Projects[project]
		delta.Policy = &approval
		result.Projects[project] = delta
	}
	for project, row := range configs {
		var config runnerauth.ProjectConfiguration
		if err := s.readConfigurationPayload(ctx, scope.Organization, eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &config); err != nil {
			return runnerauth.SyncResponse{}, err
		}
		delta := result.Projects[project]
		delta.Configuration = &config
		result.Projects[project] = delta
	}
	result.NoChange = result.Routing == nil && len(result.Projects) == 0 && cursor == request.Versions.Cursor
	if !result.NoChange {
		result.Versions = &versions
	}
	return result, nil
}

func sameSyncProjects(a, b map[tracker.ProjectID]runnerauth.SyncProjectVersion) bool {
	return maps.EqualFunc(a, b, func(runnerauth.SyncProjectVersion, runnerauth.SyncProjectVersion) bool { return true })
}
