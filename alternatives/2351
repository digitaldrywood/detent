package integrationpage

import (
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Record(record projectstore.Record) projectstore.Integration {
	v := record.Settings
	owner := "detent"
	if record.Core.Profile == "github_compatible" {
		owner = "github"
	}
	result := projectstore.Integration{Icon: iconOverride(v.Icon), Name: record.Core.Name, Profile: record.Core.Profile, Revision: tracker.Revision(v.Revision), Intake: v.Intake, ManualImportEnabled: v.Intake == "manual", Projection: v.Projection, RepositoryEnabled: v.RepositoryEnabled, Repository: v.Repository, CheckoutRepository: v.CheckoutRepository, States: record.States, WorkflowSource: record.Core.WorkflowSource, WorkflowSourceRevision: record.Core.WorkflowSourceRevision, AgingEnabled: v.AgingEnabled, DuplicateChallenge: v.DuplicateChallenge, Authority: map[string]string{"title": owner, "body": owner, "discussion": owner, "dependencies": owner, "authors": owner, "source_timestamps": "source", "workflow": owner, "labels": owner, "assignees": owner, "priority": owner, "scheduling": "detent", "progress": "detent", "repository_policy": "trusted_repository_revision", "github_merge": "github_branch_protections_and_fresh_checks", "native_approval": "detent_only"}}
	if result.Icon != nil && result.Icon.Kind == "image" {
		result.Icon.URL = "/organizations/" + string(record.Core.OrganizationID) + "/api/v2/projects/" + string(record.Core.ID) + "/attachments/" + result.Icon.ImageKey
	}
	if record.Core.WorkflowSource != "" {
		result.Authority["workflow"] = "repository"
	}
	if v.ArchiveCompletedAfterDays != nil {
		result.ArchiveCompletedAfterDays = new(int(*v.ArchiveCompletedAfterDays))
	}
	if v.ArchiveCancelledAfterDays != nil {
		result.ArchiveCancelledAfterDays = new(int(*v.ArchiveCancelledAfterDays))
	}
	if result.Profile == "native" && (v.Repository != "" || v.CheckoutRepository != "") {
		result.Intake = "automatic"
	}
	return result
}

func iconOverride(icon projecticon.Icon) *projecticon.Icon {
	if icon.Kind == "" || icon.Kind == "automatic" {
		return nil
	}
	return &icon
}
