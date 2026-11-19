package executionapi

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

type CommandDeps struct {
	Repository    tenantstore.Repository
	WriteClaim    func(context.Context, hubscope.NativeScope, func(context.Context, tenantstore.Writer) error) error
	OwnsRunEvents func() bool
}

func (d CommandDeps) ReadNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, read func(context.Context, tenantstore.Reader) error) error {
	if scope.Credential.Runner.RunnerID != "" {
		if repository, ok := d.Repository.(interface {
			ReadRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Reader) error) error
		}); ok {
			return repository.ReadRunnerEventCommand(ctx, scope.Organization, scope.Credential.ID, scope.Credential.Runner.RunnerID, command, read)
		}
	}
	if repository, ok := d.Repository.(tenantstore.SnapshotRepository); ok {
		return repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return read(ctx, r) })
	}
	return hubapi.NativeRunEventStorageUnavailable()
}

func (d CommandDeps) WriteNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	if scope.Credential.Runner.RunnerID == "" {
		return d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
	}
	optimized := command.Operation == "" || d.OwnsRunEvents()
	if repository, ok := d.Repository.(interface {
		WriteRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
	}); ok && optimized {
		return repository.WriteRunnerEventCommand(ctx, scope.Organization, scope.Credential.ID, scope.Credential.Runner.RunnerID, command, write)
	}
	return d.WriteClaim(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if _, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID); err != nil {
			return err
		}
		if command.Operation != "" && !d.OwnsRunEvents() {
			return write(ctx, w)
		}

		if facts, ok := w.(interface {
			WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
		}); ok {
			return facts.WithRunnerEventFacts(ctx, command, func(w tenantstore.Writer) error { return write(ctx, w) })
		}
		return write(ctx, w)
	})
}

func LockRunnerCredential(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error {
	if scope.Credential.Runner.RunnerID == "" {
		return nil
	}
	_, err := w.RunnerIdentityByCredential(ctx, scope.Credential.ID)
	return err
}

func RunnerEventCommandRoute(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && (c.Path() == hubapi.NativeBase+"/work-items/:item/events" || c.Path() == hubapi.NativeBase+"/runner-sessions" || c.Path() == hubapi.NativeBase+"/leases/:lease/release")
}

func (d CommandDeps) RunnerEventCommandScope(c echo.Context, scope hubscope.NativeScope) bool {
	if !RunnerEventCommandRoute(c) || scope.Credential.Runner.RunnerID == "" || scope.Credential.EntryKey != nil {
		return false
	}
	if !d.OwnsRunEvents() {
		return false
	}
	_, ok := d.Repository.(interface {
		WriteRunnerEventCommand(context.Context, tracker.OrganizationID, string, string, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
	})
	return ok
}
