package hubserver

import "github.com/digitaldrywood/detent/internal/tracker"

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: nativeIntegrationActor("health_detector")}
}
