package hubserver

import "github.com/digitaldrywood/detent/internal/tracker"

func (s *Service) artifactCatalogOrganization() tracker.OrganizationID {
	return s.accessOrganization()
}
