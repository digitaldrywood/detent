package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPublicationReusesSubjectRepairOwner(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	now := service.config.now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixtures := []nativeFixture{}
		expected := map[tracker.ProjectID]tracker.NativeWorkItemID{}
		subjects := map[tracker.ProjectID]tracker.NativeWorkItemID{}
		findings := []healthFinding{}
		for _, test := range []struct {
			name, kind string
			reverse    bool
		}{{"dependency", "dependency", false}, {"related forward", "related", false}, {"related reverse", "related", true}} {
			f := newNativeFixture(t, service, org, test.name)
			addHealthBacklog(t, f)
			var subject, owner tracker.NativeIssue
			if test.reverse {
				owner, subject = f.create(t, "Active repair"), f.create(t, "Subject")
			} else {
				subject, owner = f.create(t, "Subject"), f.create(t, "Active repair")
			}
			relationship := corestore.Relationship{OrganizationID: org, ProjectID: f.project.ID, WorkItemID: subject.WorkItemID, RelatedProjectID: f.project.ID, RelatedWorkItemID: owner.WorkItemID, Type: test.kind}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.SetRelationship(t.Context(), relationship, "add", now) }); err != nil {
				t.Fatal(err)
			}
			fixtures = append(fixtures, f)
			expected[f.project.ID] = owner.WorkItemID
			subjects[f.project.ID] = subject.WorkItemID
			findings = append(findings, healthFinding{Fingerprint: "subject:" + string(subject.WorkItemID), Signal: "runner_setup", Class: "instance", Subject: healthSubject{Kind: "work_item", ID: string(subject.WorkItemID)}, Projects: []string{string(f.project.ID)}, Severity: "attention", Summary: "The instance setup failed", NextAction: "Repair instance setup"})
		}
		for index, f := range fixtures {
			other := fixtures[(index+1)%len(fixtures)]
			foreign := other.create(t, "Foreign project repair")
			relationship := corestore.Relationship{OrganizationID: org, ProjectID: f.project.ID, WorkItemID: subjects[f.project.ID], RelatedProjectID: other.project.ID, RelatedWorkItemID: foreign.WorkItemID, Type: "dependency"}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.SetRelationship(t.Context(), relationship, "add", now) }); err != nil {
				t.Fatal(err)
			}
		}
		if err := service.publishHealthEvaluation(t.Context(), org, now, findings); err != nil {
			t.Fatal(err)
		}
		for range len(fixtures) {
			worked, err := service.maintainHealthIssue(t.Context(), org, now)
			if err != nil || !worked {
				t.Fatalf("publication worked=%t err=%v", worked, err)
			}
		}
		for _, f := range fixtures {
			page, err := service.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "all", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || len(page.Items[0].Issues) != 1 || page.Items[0].Issues[0].WorkItemID != expected[f.project.ID] {
				t.Fatalf("organization=%s project=%s active repair lost: %+v", org, f.project.ID, page)
			}
		}
	}
}
