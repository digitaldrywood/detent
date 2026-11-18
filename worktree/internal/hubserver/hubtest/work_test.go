package hubtest_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenant struct {
	organization tracker.OrganizationID
	project      tracker.NativeProject
	other        tracker.NativeProject
}

func twoTenants(t *testing.T, repository tenantstore.Repository, now time.Time) [2]tenant {
	t.Helper()
	var tenants [2]tenant
	for index := range tenants {
		organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
		tenants[index] = tenant{organization: organization, project: hubtest.Project(t, repository, organization, hubtest.ProjectOptions{}), other: hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})}
	}
	return tenants
}

func read(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, operation func(tenantstore.Reader) error) error {
	t.Helper()
	return repository.ReadTenant(t.Context(), organization, corestore.Serving, operation)
}

func TestPolicySeedsAnApprovedPolicyPerTenant(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	details := hubtest.NewDetails()
	tenants := twoTenants(t, repository, clock.Now())
	seeded := [2]hubtest.SeededPolicy{}
	for index, tenant := range tenants {
		seeded[index] = hubtest.Policy(t, repository, details, tenant.project, hubtest.PolicyOptions{Now: clock.Now()})
	}
	for index, tenant := range tenants {
		if err := read(t, repository, tenant.organization, func(r tenantstore.Reader) error {
			current, err := r.ProjectPolicy(t.Context(), tenant.project.ID)
			if err != nil {
				return err
			}
			if current.PolicyID != hubtest.PolicyDescriptor().ID || current.ApprovedBy != "fixture" || !current.ApprovedAt.Equal(clock.Now()) || current.OrganizationID != tenant.organization || current.PayloadObjectKey != seeded[index].Revision.PayloadObjectKey {
				t.Errorf("approved policy = %+v", current)
			}
			descriptor, err := eventstore.NewContent(details).ProjectPolicy(t.Context(), current)
			if err != nil {
				return err
			}
			if descriptor.ID != seeded[index].Descriptor.ID || descriptor.Gates.MergeMethod != "squash" {
				t.Errorf("stored descriptor = %+v", descriptor)
			}
			if _, err := r.ProjectPolicy(t.Context(), tenant.other.ID); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("unseeded project policy = %v, want not found", err)
			}
			if _, err := r.ProjectPolicy(t.Context(), tenants[1-index].project.ID); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("other tenant policy = %v, want not found", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Minute)
	again := hubtest.Policy(t, repository, details, tenants[0].project, hubtest.PolicyOptions{Now: clock.Now()})
	if again.Revision.PolicyID != seeded[0].Revision.PolicyID || !again.Revision.ApprovedAt.Equal(seeded[0].Revision.ApprovedAt) {
		t.Fatalf("second seed replaced the approved policy: %+v", again.Revision)
	}
	replacement := hubtest.PolicyDescriptor()
	replacement.Gates.MergeMethod = "merge"
	replaced := hubtest.Policy(t, repository, details, tenants[0].project, hubtest.PolicyOptions{Descriptor: &replacement, ApprovedBy: "example-admin", Now: clock.Now()})
	if replaced.Descriptor.ID == seeded[0].Descriptor.ID || replaced.Descriptor.ID != replacement.WithID().ID {
		t.Fatalf("replacement policy ID = %q", replaced.Descriptor.ID)
	}
	for index, want := range []policy.Descriptor{replaced.Descriptor, seeded[1].Descriptor} {
		if err := read(t, repository, tenants[index].organization, func(r tenantstore.Reader) error {
			current, err := r.ProjectPolicy(t.Context(), tenants[index].project.ID)
			if err == nil && current.PolicyID != want.ID {
				t.Errorf("tenant %d current policy = %q, want %q", index, current.PolicyID, want.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIssueSeedsWorkItemsInWorkflowStates(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	details := hubtest.NewDetails()
	tenants := twoTenants(t, repository, clock.Now())
	priority := 2
	type seededIssue struct {
		tenant   tenant
		issue    hubtest.SeededIssue
		state    string
		number   int
		waiting  bool
		priority *int
	}
	var seeded []seededIssue
	for _, tenant := range tenants {
		for index, test := range []struct {
			options hubtest.IssueOptions
			state   string
			waiting bool
		}{
			{hubtest.IssueOptions{Now: clock.Now()}, "Todo", true},
			{hubtest.IssueOptions{State: "In Progress", Title: "Example started", Labels: []string{"example"}, Assignees: []string{"example-assignee"}, Priority: &priority, Now: clock.Now()}, "In Progress", true},
			{hubtest.IssueOptions{State: "Done", Now: clock.Now()}, "Done", false},
		} {
			seeded = append(seeded, seededIssue{tenant: tenant, issue: hubtest.Issue(t, repository, details, tenant.project, test.options), state: test.state, number: index + 1, waiting: test.waiting, priority: test.options.Priority})
		}
	}
	for _, want := range seeded {
		issue := want.issue
		if err := read(t, repository, want.tenant.organization, func(r tenantstore.Reader) error {
			row, err := r.WorkItem(t.Context(), want.tenant.project.ID, issue.Row.NativeID)
			if err != nil {
				return err
			}
			if row.ID != issue.Row.ID || row.Number != int64(want.number) || row.Revision != 1 || row.WorkflowStateName == nil || *row.WorkflowStateName != want.state || row.Title != issue.Issue.Title || row.BodyObjectKey == nil || !row.CreatedAt.Equal(clock.Now()) {
				t.Errorf("work item = %+v", row)
			}
			facts, err := r.WorkItemFacts(t.Context(), want.tenant.project.ID, issue.Row.NativeID)
			if err != nil {
				return err
			}
			if (facts.DispatchWaitSince != nil) != want.waiting || facts.Author != "example-owner" || len(facts.Assignees) != len(issue.Issue.Assignees) {
				t.Errorf("claim facts = %+v, want waiting %t", facts, want.waiting)
			}
			queue, err := r.QueueEntry(t.Context(), want.tenant.project.ID, issue.Row.NativeID, string(want.tenant.project.ID))
			if err != nil {
				return err
			}
			if queue.State != want.state || queue.Rank != string(issue.Row.NativeID) || (queue.Priority == nil) != (want.priority == nil) || queue.Priority != nil && *queue.Priority != int64(*want.priority) {
				t.Errorf("queue entry = %+v", queue)
			}
			version, err := r.CollaborationVersion(t.Context(), string(issue.Row.NativeID), 1)
			if err != nil {
				return err
			}
			if _, err := eventstore.NewContent(details).CollaborationVersion(t.Context(), version); err != nil {
				t.Errorf("stored issue version: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		foreign := tenants[0].organization
		if foreign == want.tenant.organization {
			foreign = tenants[1].organization
		}
		if err := read(t, repository, foreign, func(r tenantstore.Reader) error {
			_, err := r.WorkItemByNativeID(t.Context(), issue.Row.NativeID)
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("other tenant read of %s = %v, want not found", issue.Row.NativeID, err)
		}
		if issue.Issue.Terminal != (want.state == "Done") || (issue.Issue.ClosedAt != nil) != (want.state == "Done") || issue.Issue.Number != want.number {
			t.Fatalf("seeded issue = %+v", issue.Issue)
		}
	}
}

func TestRunnerSeedsEnrolledIdentitiesInBothScopes(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	tenants := twoTenants(t, repository, clock.Now())
	type runners struct{ selected, organization, bare hubtest.SeededRunner }
	var seeded [2]runners
	for index, tenant := range tenants {
		seeded[index] = runners{
			selected:     hubtest.Runner(t, repository, tenant.organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{tenant.project.ID}, DisplayName: "Example selected runner", Capacity: 3, Now: clock.Now()}),
			organization: hubtest.Runner(t, repository, tenant.organization, hubtest.RunnerOptions{Organization: true, Now: clock.Now()}),
			bare:         hubtest.Runner(t, repository, tenant.organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{tenant.project.ID}, WithoutCapacity: true, Now: clock.Now()}),
		}
	}
	for index, tenant := range tenants {
		for _, test := range []struct {
			name     string
			runner   hubtest.SeededRunner
			scope    string
			display  string
			limit    int64
			capacity bool
			projects int
		}{
			{"selected", seeded[index].selected, "projects", "Example selected runner", 3, true, 1},
			{"organization", seeded[index].organization, "organization", "Example runner", 1, true, 0},
			{"without capacity", seeded[index].bare, "projects", "Example runner", 1, false, 1},
		} {
			if err := read(t, repository, tenant.organization, func(r tenantstore.Reader) error {
				record, err := r.RunnerIdentityByCredential(t.Context(), test.runner.Credential.ID)
				if err != nil {
					return err
				}
				if record.RunnerID != test.runner.Identity.RunnerID || record.MachineID != test.runner.Identity.MachineID || record.Scope != test.scope || record.DisplayName != test.display || record.CapacityLimit != test.limit || record.ReportedCapacity != test.limit || record.State != "active" || len(test.runner.Identity.ProjectIDs) != test.projects || test.runner.Identity.Scope != test.scope || record.RemovedAt != nil {
					t.Errorf("%s runner identity = %+v", test.name, record)
				}
				if (record.CapacityConfig != nil) != test.capacity || (test.runner.CapacityConfig != nil) != test.capacity {
					t.Errorf("%s runner capacity configuration = %+v", test.name, record.CapacityConfig)
				}
				if test.capacity && (record.CapacityConfig.LocalLimit != int(test.limit) || record.CapacityConfig.Validate() != nil) {
					t.Errorf("%s runner capacity configuration = %+v", test.name, record.CapacityConfig)
				}
				credential, err := r.AccessCredential(t.Context(), test.runner.Credential.ID)
				if err != nil {
					return err
				}
				if credential.Scope != "worker" || credential.Kind != "service" || !credential.Valid(test.runner.Credential.Hash, clock.Now()) {
					t.Errorf("%s runner credential = %+v", test.name, credential)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	home, away := tenants[0], tenants[1]
	for _, test := range []struct {
		name    string
		tenant  tracker.OrganizationID
		runner  hubtest.SeededRunner
		project tracker.ProjectID
		denied  bool
	}{
		{"selected runner on its project", home.organization, seeded[0].selected, home.project.ID, false},
		{"selected runner on an unselected project", home.organization, seeded[0].selected, home.other.ID, true},
		{"organization runner on the first project", home.organization, seeded[0].organization, home.project.ID, false},
		{"organization runner on the second project", home.organization, seeded[0].organization, home.other.ID, false},
		{"organization runner on another tenant's project", home.organization, seeded[0].organization, away.project.ID, true},
		{"another tenant's runner", home.organization, seeded[1].organization, home.project.ID, true},
		{"second tenant selected runner at home", away.organization, seeded[1].selected, away.project.ID, false},
		{"second tenant organization runner at home", away.organization, seeded[1].organization, away.other.ID, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := hubtest.Scope(t, hubtest.ScopeFacts{Credential: test.runner.Credential, Project: test.project, Runner: test.runner.Identity, Now: clock.Now})
			if scope.Runner.RunnerID != test.runner.Identity.RunnerID {
				t.Fatalf("scope runner = %+v", scope.Runner)
			}
			err := read(t, repository, test.tenant, func(r tenantstore.Reader) error { return scope.Authorize(t.Context(), r, true) })
			if test.denied != errors.Is(err, accessstore.ErrDenied) || !test.denied && err != nil {
				t.Fatalf("Authorize = %v, want denied %t", err, test.denied)
			}
		})
	}
}

func TestLeaseSeedsAnActiveFencedClaim(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	details := hubtest.NewDetails()
	tenants := twoTenants(t, repository, clock.Now())
	type claimed struct {
		tenant tenant
		runner hubtest.SeededRunner
		rival  hubtest.SeededRunner
		issue  hubtest.SeededIssue
		policy hubtest.SeededPolicy
		lease  tracker.NativeLease
	}
	var seeded [2]claimed
	for index, tenant := range tenants {
		approved := hubtest.Policy(t, repository, details, tenant.project, hubtest.PolicyOptions{Now: clock.Now()})
		runner := hubtest.Runner(t, repository, tenant.organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{tenant.project.ID}, Now: clock.Now()})
		rival := hubtest.Runner(t, repository, tenant.organization, hubtest.RunnerOptions{Organization: true, Now: clock.Now()})
		issue := hubtest.Issue(t, repository, details, tenant.project, hubtest.IssueOptions{Now: clock.Now()})
		lease := hubtest.Lease(t, repository, runner, issue, hubtest.LeaseOptions{TTL: 30 * time.Minute, PolicyID: approved.Descriptor.ID, Now: clock.Now()})
		seeded[index] = claimed{tenant: tenant, runner: runner, rival: rival, issue: issue, policy: approved, lease: lease}
	}
	if seeded[0].lease.ID == seeded[1].lease.ID {
		t.Fatal("two tenants share a lease ID")
	}
	for index, want := range seeded {
		if want.lease.FencingToken < 1 || want.lease.WorkItemID != want.issue.Row.NativeID || want.lease.MachineID != want.runner.Identity.MachineID || !want.lease.ExpiresAt.Equal(clock.Now().Add(30*time.Minute)) || want.lease.PolicyID != want.policy.Descriptor.ID {
			t.Fatalf("seeded lease = %+v", want.lease)
		}
		if err := read(t, repository, want.tenant.organization, func(r tenantstore.Reader) error {
			active, err := r.Lease(t.Context(), string(want.lease.ID))
			if err != nil {
				return err
			}
			if active.IssueID != want.issue.Row.ID || active.FencingToken != int64(want.lease.FencingToken) || active.SessionID != want.lease.SessionID || active.ReleasedAt != nil || active.WorkItemRevision != want.issue.Row.Revision {
				t.Errorf("active lease = %+v", active)
			}
			owner, err := r.RunnerForLease(t.Context(), string(want.lease.ID))
			if err != nil {
				return err
			}
			if owner.RunnerID != want.runner.Identity.RunnerID {
				t.Errorf("lease runner = %q, want %q", owner.RunnerID, want.runner.Identity.RunnerID)
			}
			bound, err := r.LeasePolicy(t.Context(), want.lease.ID)
			if err != nil {
				return err
			}
			if bound.PolicyID != want.policy.Descriptor.ID || bound.ProjectID != want.tenant.project.ID {
				t.Errorf("lease policy = %+v", bound)
			}
			if _, err := r.Lease(t.Context(), string(seeded[1-index].lease.ID)); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("other tenant lease = %v, want not found", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		err := repository.WriteTenant(t.Context(), want.tenant.organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Claim(t.Context(), leasestore.Claim{IssueID: want.issue.Row.ID, ExpectedRevision: want.issue.Row.Revision, MachineID: string(want.rival.Identity.MachineID), SessionID: "execution_rival", LeaseID: "lease_rival", TTL: time.Minute, RunnerID: want.rival.Identity.RunnerID})
			return err
		})
		if !errors.Is(err, tracker.ErrLeaseConflict) {
			t.Fatalf("rival claim = %v, want lease conflict", err)
		}
	}
	clock.Advance(31 * time.Minute)
	second := hubtest.Lease(t, repository, seeded[0].rival, seeded[0].issue, hubtest.LeaseOptions{Now: clock.Now()})
	if second.FencingToken <= seeded[0].lease.FencingToken || second.PolicyID != "" {
		t.Fatalf("lease after expiry = %+v, previous fencing token %d", second, seeded[0].lease.FencingToken)
	}
}

func TestChangeSeedsOnePublishedVersion(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repository := hubtest.Repository(t, hubtest.WithClock(clock.Now))
	details := hubtest.NewDetails()
	tenants := twoTenants(t, repository, clock.Now())
	type published struct {
		tenant tenant
		issue  hubtest.SeededIssue
		change hubtest.SeededChange
	}
	var seeded [2]published
	for index, tenant := range tenants {
		approved := hubtest.Policy(t, repository, details, tenant.project, hubtest.PolicyOptions{Now: clock.Now()})
		issue := hubtest.Issue(t, repository, details, tenant.project, hubtest.IssueOptions{State: "In Progress", Now: clock.Now()})
		seeded[index] = published{tenant: tenant, issue: issue, change: hubtest.Change(t, repository, details, issue, approved, hubtest.ChangeOptions{Now: clock.Now()})}
	}
	content := eventstore.NewContent(details)
	for index, want := range seeded {
		change := want.change
		if change.Change.CurrentVersion != change.Version.ID || change.Version.Number != 1 || change.Change.Revision != 2 || change.Version.PolicyID != hubtest.PolicyDescriptor().ID {
			t.Fatalf("seeded change = %+v version = %+v", change.Change, change.Version)
		}
		if err := read(t, repository, want.tenant.organization, func(r tenantstore.Reader) error {
			row, err := r.Change(t.Context(), change.Change.ID)
			if err != nil {
				return err
			}
			if row.CurrentVersionID == nil || *row.CurrentVersionID != change.Version.ID || row.WorkItemID != want.issue.Row.NativeID || row.Revision != 2 || row.LandedVersionID != nil {
				t.Errorf("change row = %+v", row)
			}
			stored, err := content.Change(t.Context(), row)
			if err != nil {
				return err
			}
			if stored.CurrentVersion != change.Version.ID || stored.Title != want.issue.Row.Title || len(stored.LinkedIssues) != 1 || stored.LinkedIssues[0] != want.issue.Row.NativeID {
				t.Errorf("stored change = %+v", stored)
			}
			latest, err := r.LatestChange(t.Context(), want.tenant.project.ID, want.issue.Row.NativeID)
			if err != nil {
				return err
			}
			if latest.ID != change.Change.ID {
				t.Errorf("latest change = %q, want %q", latest.ID, change.Change.ID)
			}
			versions, err := r.Versions(t.Context(), change.Change.ID, 0, 10)
			if err != nil {
				return err
			}
			if len(versions) != 1 || versions[0].ID != change.Version.ID || versions[0].HeadSHA != change.Version.HeadSHA || versions[0].PolicyRevisionID != change.Version.PolicyID {
				t.Errorf("versions = %+v", versions)
			}
			if len(versions) == 1 {
				version, err := content.Version(t.Context(), versions[0])
				if err != nil {
					return err
				}
				if version.Code.Kind != "code" || version.Repository != "https://github.com/example/repo" || version.Policy.ID != change.Version.PolicyID {
					t.Errorf("stored version = %+v", version)
				}
			}
			if _, err := r.Change(t.Context(), seeded[1-index].change.Change.ID); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("other tenant change = %v, want not found", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
