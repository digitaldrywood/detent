package hubserver

import "github.com/labstack/echo/v4"

func (s *Service) registerMonthlyBudgetWriteRoutes(e *echo.Echo) {
	e.PUT(hostedOrganizationBase+"/monthly-budget", s.organizationMonthlyBudgetScope, s.hostedAdministrationSession)
}
