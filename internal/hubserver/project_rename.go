package hubserver

import (
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/projectadmin"
)

type renameProjectRequest = projectadmin.RenameRequest

func (s *Service) renameProject(c echo.Context) error {
	return projectadmin.Rename(c, projectadmin.RenameDeps{
		InvalidRequest: s.invalidAPIRequest,
		APIError:       s.nativeAPIError,
		Mutate: func(c echo.Context, request projectadmin.RenameRequest) error {
			return s.nativeMutation(c, request.Mutation, request, nil)
		},
	})
}
