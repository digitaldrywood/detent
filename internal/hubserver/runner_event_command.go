package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeRunCommand(ctx context.Context, scope nativeScope, command eventstore.RunCommand, read func(context.Context, tenantstore.Reader) error) error {
	if scope.credential.Runner.RunnerID != "" {
		if repository, ok := s.config.TenantRepository.(interface {
			ReadRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Reader) error) error
		}); ok {
			return repository.ReadRunnerEventCommand(ctx, scope.organization, scope.credential.ID, scope.credential.Runner.RunnerID, command, read)
		}
	}
	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		return repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { return read(ctx, r) })
	}
	return nativeRunEventStorageUnavailable()
}

func (s *Service) writeNativeRunCommand(ctx context.Context, scope nativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	if scope.credential.Runner.RunnerID == "" {
		return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
	}
	optimized := command.Operation == ""
	switch s.config.NativeRunEvents.(type) {
	case tenantNativeRunEventParticipant, *tenantNativeRunEventParticipant:
		optimized = true
	}
	if repository, ok := s.config.TenantRepository.(interface {
		WriteRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
	}); ok && optimized {
		return repository.WriteRunnerEventCommand(ctx, scope.organization, scope.credential.ID, scope.credential.Runner.RunnerID, command, write)
	}
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
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

func (s *Service) runnerEventCommandScope(c echo.Context, scope nativeScope) bool {
	if c.Request().Method != http.MethodPost || scope.credential.Runner.RunnerID == "" || scope.credential.EntryKey != nil {
		return false
	}
	if c.Path() != nativeBase+"/work-items/:item/events" && c.Path() != nativeBase+"/runner-sessions" && c.Path() != nativeBase+"/leases/:lease/release" {
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
