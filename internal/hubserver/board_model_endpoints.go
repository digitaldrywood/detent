package hubserver

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) boardModelEndpoint(c echo.Context, status bool) error {
	scope := nativeRequestScope(c)
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	selection, err := boardRecordSelection(scope, params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	selection.SearchText = params.Get("q")
	selection.WorkItemIDs = nil
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var response any
	read := func(ctx context.Context) error {
		return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
				return err
			}
			project, err := reader.Project(ctx, scope.project)
			if err != nil {
				return err
			}
			if project.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if status {
				for _, id := range params["item"] {
					selection.WorkItemIDs = append(selection.WorkItemIDs, tracker.NativeWorkItemID(id))
				}
				model, ok := reader.(interface {
					BoardStatusModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardStatusRecord, error)
				})
				if !ok {
					return workItemStorageUnavailable()
				}
				rows, readErr := model.BoardStatusModel(ctx, selection)
				if readErr != nil {
					return readErr
				}
				authorizer, ok := reader.(interface {
					BoardAuthorizedItems(context.Context, accessstore.Authority, []tracker.NativeWorkItemID, []string, string) (viewstore.BoardReadAuthority, error)
				})
				if !ok {
					return workItemStorageUnavailable()
				}
				ids := make([]tracker.NativeWorkItemID, 0, len(rows))
				changes := []string{}
				for _, row := range rows {
					ids = append(ids, row.WorkItemID)
					if row.Change != nil {
						changes = append(changes, row.Change.ID)
					}
				}
				reporter := ""
				if selection.Reporter {
					reporter = selection.ReporterPrincipalID
				}
				allowed, err := authorizer.BoardAuthorizedItems(ctx, s.accessAuthority(scope, false), ids, changes, reporter)
				if err != nil {
					return err
				}
				visible := make([]viewstore.BoardStatusRecord, 0, len(rows))
				for _, row := range rows {
					if allowed.Items[row.WorkItemID] != scope.project {
						continue
					}
					if row.Change != nil && !allowed.Changes[row.Change.ID] {
						row.Change = nil
					}
					visible = append(visible, row)
				}
				response = visible
			} else {
				model, ok := reader.(interface {
					BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
				})
				if !ok {
					return workItemStorageUnavailable()
				}
				response, err = model.BoardSummaryModel(ctx, selection)
			}
			return err
		})
	}
	if repository, ok := s.config.TenantRepository.(interface {
		ReadBoardRequest(context.Context, tracker.OrganizationID, string, func(context.Context) error) error
	}); ok {
		err = repository.ReadBoardRequest(ctx, scope.organization, scope.credential.ID, read)
	} else {
		err = read(ctx)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) boardModelCounts(c echo.Context) error { return s.boardModelEndpoint(c, false) }
func (s *Service) boardModelStatus(c echo.Context) error { return s.boardModelEndpoint(c, true) }
