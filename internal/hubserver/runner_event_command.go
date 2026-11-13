package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, read func(context.Context, tenantstore.Reader) error) error {
	if scope.Credential.Runner.RunnerID != "" {
		if repository, ok := s.config.TenantRepository.(interface {
			ReadRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Reader) error) error
		}); ok {
			return repository.ReadRunnerEventCommand(ctx, scope.Organization, scope.Credential.ID, scope.Credential.Runner.RunnerID, command, read)
		}
	}
	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		return repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return read(ctx, r) })
	}
	return hubapi.NativeRunEventStorageUnavailable()
}

func (s *Service) writeNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	if scope.Credential.Runner.RunnerID == "" {
		return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
	}
	optimized := command.Operation == ""
	switch s.config.NativeRunEvents.(type) {
	case tenantNativeRunEventParticipant, *tenantNativeRunEventParticipant:
		optimized = true
	}
	if repository, ok := s.config.TenantRepository.(interface {
		WriteRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
	}); ok && optimized {
		return repository.WriteRunnerEventCommand(ctx, scope.Organization, scope.Credential.ID, scope.Credential.Runner.RunnerID, command, write)
	}
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if _, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID); err != nil {
			return err
		}
		if command.Operation != "" {
			switch s.config.NativeRunEvents.(type) {
			case tenantNativeRunEventParticipant, *tenantNativeRunEventParticipant:
			default:
				return write(ctx, w)
			}
		}

		if facts, ok := w.(interface {
			WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
		}); ok {
			return facts.WithRunnerEventFacts(ctx, command, func(w tenantstore.Writer) error { return write(ctx, w) })
		}
		return write(ctx, w)
	})
}

type runnerEventAuthorityKey struct{}

func runnerEventCommandRoute(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && (c.Path() == hubapi.NativeBase+"/work-items/:item/events" || c.Path() == hubapi.NativeBase+"/runner-sessions" || c.Path() == hubapi.NativeBase+"/leases/:lease/release")
}

func (s *Service) runnerEventCommandScope(c echo.Context, scope hubscope.NativeScope) bool {
	if !runnerEventCommandRoute(c) || scope.Credential.Runner.RunnerID == "" || scope.Credential.EntryKey != nil {
		return false
	}
	switch s.config.NativeRunEvents.(type) {
	case tenantNativeRunEventParticipant, *tenantNativeRunEventParticipant:
	default:
		return false
	}
	_, ok := s.config.TenantRepository.(interface {
		WriteRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
	})
	return ok
}
