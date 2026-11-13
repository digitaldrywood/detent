package hubauth

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeOrganizationIssuePath = "/api/v2/organizations/:organization/work-items/:item"

func (s *Authenticator) ResolveNative(readCtx context.Context, c echo.Context, options NativeOptions) error {
	credential, ok := options.Credential(c)
	if !ok {
		return hubapi.NotFound()
	}
	scope := Input{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project")), Credential: credential}
	if options.EventScope(c, scope) {
		if credential.Runner.OrganizationID != scope.Organization {
			return hubapi.NotFound()
		}
		options.Store(c, scope)
		return nil
	}
	if c.Path() == nativeOrganizationIssuePath {
		var item corestore.WorkItem
		err := s.deps.Repository.ReadTenant(readCtx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			item, err = r.WorkItemByNativeID(readCtx, tracker.NativeWorkItemID(c.Param("item")))
			return err
		})
		if err != nil {
			return options.RecordError(err)
		}
		scope.Project = item.ProjectID
	}
	runnerHeartbeat := c.Path() == nativeBase+"/machines/:machine/heartbeat" && credential.Runner.RunnerID != ""
	if runnerHeartbeat {
		if credential.Runner.OrganizationID != scope.Organization {
			return hubapi.NotFound()
		}
	} else {
		write := !ReadRequest(c) && !options.ArtifactRead(c) && !options.RelayTicket(c) && c.Path() != nativeBase+"/runner-sessions/find"
		if write && c.Request().Method == http.MethodPost && c.Path() == nativeBase+"/work-items/:item/workflow" {
			scope.Action = operatortool.MoveItem
		}
		if write && credential.HostedRole == "reporter" {
			switch c.Request().Method + " " + c.Path() {
			case "POST " + nativeBase + "/release-audits":
				return hubapi.NotFound()
			case "PATCH " + nativeBase + "/work-items/:item":
				scope.Action = operatortool.EditItem
			case "POST " + nativeBase + "/work-items":
				scope.Action = operatortool.FileIssue
			case "POST " + nativeBase + "/work-items/:item/comments":
				scope.Action = operatortool.AddComment
			case "POST " + nativeBase + "/work-items/:item/dependencies":
				scope.Action = operatortool.SetDependency
			case "PATCH " + nativeBase + "/work-items/:item/comments/:comment":
				scope.Action = operatortool.EditComment
			case "POST " + nativeBase + "/work-items/:item/workflow":
				scope.Action = operatortool.MoveItem
			case "POST " + nativeBase + "/work-items/:item/test-items":
			case "POST " + nativeBase + "/work-items/:item/verification":
				scope.Action = operatortool.VerifyFix
			case "POST " + attachmentMetadataBase, "POST " + attachmentMetadataBase + "/check":
				scope.Action = operatortool.UploadAttachment
			case "POST " + attachmentMetadataBase + "/:attachment/reference":
				scope.Action = operatortool.ReferenceAttachment
			}
			if !RoleAllows(credential.HostedRole, apikey.ScopeWrite, scope.Action) {
				return operatortool.ErrAccessDenied
			}
		}
		if options.LeaseRoute(c.Path()) {
			if err := options.AuthorizeLease(readCtx, scope); err != nil {
				return err
			}
		} else if options.RepositoryRoute(c.Path()) && credential.Runner.RunnerID != "" {
			if err := options.AuthorizeRepository(readCtx, scope); err != nil {
				return err
			}
		} else {
			err := options.ReadProject(readCtx, scope, write, c.Request().Method == http.MethodGet && c.Path() == nativeBase)
			if err != nil {
				return err
			}
		}
	}
	if item := c.Param("item"); item != "" {
		visibilityErr := options.ReadItem(readCtx, scope, item)
		if err := visibilityErr; err != nil {
			return err
		}
	}
	options.Store(c, scope)
	return nil
}
