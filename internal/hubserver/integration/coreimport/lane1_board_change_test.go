package coreimport_test

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemchange"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnsSelectIssueSideData(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, test := range []struct {
		name string
		pull bool
	}{
		{name: "current version"},
		{name: "version head"},
		{name: "current landing"},
		{name: "old landing"},
		{name: "external reference", pull: true},
		{name: "external provider"},
		{name: "empty external URL"},
	} {
		for _, historical := range []bool{false, true} {
			if test.name == "version head" && historical {
				continue
			}
			t.Run(fmt.Sprintf("%s/historical=%v", test.name, historical), func(t *testing.T) {
				f := hubfixture.NewChangeInOrganization(t, h)
				old := f.Publish(t, "old", "")
				current := f.Publish(t, "current", old.ID)
				head, branch := current.HeadSHA, ""
				switch test.name {
				case "current version":
					head = old.HeadSHA
				case "version head":
					head = "typed-head"
				case "current landing":
					head, branch = "typed-merge", "typed-base"
				}
				f = changeColumnSideDataFixture(t, f, old, current, test.name, historical)
				var issue tracker.NativeIssue
				if test.name == "version head" {
					issue = prepareBoardChangeIssue(t, f)
				} else {
					issue = hubfixture.ReadWorkItem(t, f.Native, f.Issue.WorkItemID, "?include=change")
				}
				if issue.WorkItemID != f.Issue.WorkItemID || issue.Change == nil || issue.Change.ChangeID != f.Change.ID {
					t.Fatalf("work item=%s Change=%+v want work item=%s Change=%s", issue.WorkItemID, issue.Change, f.Issue.WorkItemID, f.Change.ID)
				}
				if issue.ListChange == nil || issue.ListChange.HeadSHA != head || issue.ListChange.Branch != branch {
					t.Fatalf("board Change=%+v want branch=%q head=%q", issue.ListChange, branch, head)
				}
				if (issue.PullRequest != nil) != test.pull || test.pull && (issue.PullRequest.Number != 42 || issue.PullRequest.URL != "https://example.test/pull/42") {
					t.Fatalf("pull request=%+v want GitHub pull request 42 present=%v", issue.PullRequest, test.pull)
				}
			})
		}
	}
}

func prepareBoardChangeIssue(t *testing.T, f hubfixture.Change) tracker.NativeIssue {
	t.Helper()
	var snapshot workitemchange.Snapshot
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		project, err := r.ProjectRecord(t.Context(), f.Project.ID)
		if err != nil {
			return err
		}
		change, err := r.LatestChange(t.Context(), f.Project.ID, f.Issue.WorkItemID)
		if err != nil {
			return err
		}
		if change.CurrentVersionID == nil {
			return corestore.ErrNotFound
		}
		version, err := r.Version(t.Context(), change.ID, *change.CurrentVersionID)
		snapshot.Project, snapshot.Change, snapshot.ChangeVersion = project, &change, &version
		return err
	}); err != nil {
		t.Fatal(err)
	}
	issue := f.Issue
	workitemchange.New(workitemchange.Deps{PullRequestNumber: pullrequests.ExternalNumber}).Prepare(snapshot, &issue)
	return issue
}

func changeColumnSideDataFixture(t *testing.T, f hubfixture.Change, old, current tracker.ChangeVersion, scenario string, historical bool) hubfixture.Change {
	t.Helper()
	content := eventstore.NewContent(f.Hub.Config.RunDetails)
	external := func(version *tracker.ChangeVersion) {
		switch scenario {
		case "external reference":
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://example.test/pull/42"}
		case "external provider":
			version.External = &tracker.ChangeExternalReference{Provider: "other", ID: "42", URL: "https://example.test/pull/42"}
		case "empty external URL":
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: ""}
		}
	}
	if historical {
		fixture, target, versions := historyRepairSource(t, f, old, 2)
		versions[1].HeadSHA = current.HeadSHA
		external(&versions[1])
		now := f.Hub.Now()
		target.Change.CreatedAt, target.Change.UpdatedAt = now, now
		if scenario == "current version" {
			target.Change.CurrentVersion = versions[0].ID
		}
		if scenario == "current landing" || scenario == "old landing" {
			landed := versions[1]
			if scenario == "old landing" {
				landed = versions[0]
			}
			target.Change.Landed = &tracker.ChangeLanding{VersionID: landed.ID, HeadSHA: landed.HeadSHA, MergeSHA: "typed-merge", BaseRef: "typed-base", LandedAt: now}
		}
		fixture.Records["change_requests"] = nil
		fixture.Add("change_requests", map[string]any{"id": target.Change.ID, "organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": f.Issue.WorkItemID, "created_at": nil, "record_json": string(historyTestJSON(t, target.Change))})
		fixture.Records["change_versions"] = nil
		for _, version := range versions {
			fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": target.Change.ID, "number": version.Number, "policy_revision_id": version.PolicyID, "head_sha": nil, "record_json": string(historyTestJSON(t, version))})
		}
		if _, err := coreimport.ImportCoreChanges(t.Context(), f.Hub.Repository, historyRepairImportSource(t, target, fixture)); err != nil {
			t.Fatal(err)
		}
		return target
	}
	current.ID, current.Number = hubapi.NewID("version"), current.Number+1
	row, err := content.PrepareVersion(t.Context(), f.Project.OrganizationID, historyTestJSON(t, current))
	if err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "version head":
		row.HeadSHA = "typed-head"
	case "external reference":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "github", "42", "https://example.test/pull/42"
	case "external provider":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "other", "42", "https://example.test/pull/42"
	case "empty external URL":
		row.ExternalProvider, row.ExternalID, row.ExternalURL = "github", "42", ""
	}
	var record eventstore.Change
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Change(t.Context(), f.Change.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	change, err := content.Change(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	change.CurrentVersion = current.ID
	changed, err := content.PrepareChange(t.Context(), f.Project.OrganizationID, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "current version" {
		changed.CurrentVersionID = new(old.ID)
	}
	if scenario == "current landing" || scenario == "old landing" {
		landed := current
		if scenario == "old landing" {
			landed = old
		}
		changed.LandedVersionID, changed.LandedHeadSHA = new(landed.ID), new(landed.HeadSHA)
		changed.LandedMergeSHA, changed.LandedBaseRef = new("typed-merge"), new("typed-base")
		changed.LandedAt = new(f.Hub.Now())
	}
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		if _, err := w.CreateVersion(t.Context(), row); err != nil {
			return err
		}
		_, err := w.UpdateChange(t.Context(), changed)
		return err
	})
	return f
}
