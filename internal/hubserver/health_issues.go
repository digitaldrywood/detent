package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) hubscope.NativeScope {
	return hubscope.NativeScope{Organization: organization, Project: project, SourceActor: nativeIntegrationActor("health_detector")}
}
