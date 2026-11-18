package hubtest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func LegacyPolicyDescriptor() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func PolicyDescriptor(configure ...func(*config.Workflow)) policy.Descriptor {
	cfg := config.Default().ForNativeTracker()
	cfg.Gate.AutomatedReview = gate.AutomatedReviewOptional
	cfg.Gate.SecurityAudit.Enabled = false
	workflow := config.Workflow{Config: cfg}
	for _, apply := range configure {
		apply(&workflow)
	}
	descriptor, err := config.ResolvePolicy(workflow)
	if err != nil {
		panic(err)
	}
	return descriptor
}

func PolicyWithRequirements(requirements policy.Requirements) policy.Descriptor {
	if requirements.RunnerID == "" && requirements.MachineID == "" && len(requirements.RequiredTags) == 0 {
		return PolicyDescriptor()
	}
	return PolicyDescriptor(func(w *config.Workflow) {
		w.Config.Runners = config.Runners{Profile: "selected", Profiles: map[string]policy.Requirements{"selected": requirements}}
	})
}

func WithPolicyPrompt(prompt string) func(*config.Workflow) {
	return func(w *config.Workflow) { w.Prompt, w.SharedPrompt = prompt, prompt }
}

type PolicyOptions struct {
	Descriptor *policy.Descriptor
	ApprovedBy string
	Now        time.Time
}

type SeededPolicy struct {
	Descriptor policy.Descriptor
	Revision   policystore.Revision
}

func Policy(t *testing.T, repository tenantstore.Repository, details attachment.DetailStore, project tracker.NativeProject, options PolicyOptions) SeededPolicy {
	t.Helper()
	content := eventstore.NewContent(details)
	var current policystore.Revision
	err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		current, err = r.ProjectPolicy(t.Context(), project.ID)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("seed policy: read current: %v", err)
	}
	found := err == nil
	if found && options.Descriptor == nil {
		descriptor, err := content.ProjectPolicy(t.Context(), current)
		if err != nil {
			t.Fatalf("seed policy: read approved descriptor: %v", err)
		}
		return SeededPolicy{Descriptor: descriptor, Revision: current}
	}
	var descriptor policy.Descriptor
	if options.Descriptor == nil {
		descriptor = PolicyDescriptor()
	} else {
		descriptor = options.Descriptor.WithID()
	}
	revision, err := content.PrepareProjectPolicy(t.Context(), project.OrganizationID, descriptor)
	if err != nil {
		t.Fatalf("seed policy: prepare: %v", err)
	}
	revision.ProjectID = project.ID
	revision.ApprovedBy = options.ApprovedBy
	if revision.ApprovedBy == "" {
		revision.ApprovedBy = "fixture"
	}
	revision.ApprovedAt = seedTime(options.Now)
	expected := ""
	if found {
		expected = current.PolicyID
	}
	write(t, repository, project.OrganizationID, "policy", func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), revision, expected)
	})
	return SeededPolicy{Descriptor: descriptor, Revision: revision}
}

type IssueOptions struct {
	Title     string
	Body      string
	State     string
	Actor     tracker.Actor
	Labels    []string
	Assignees []string
	Priority  *int
	Now       time.Time
}

type SeededIssue struct {
	Issue   tracker.NativeIssue
	Row     corestore.WorkItem
	Version eventstore.CollaborationVersion
}

func dispatchable(project tracker.NativeProject, state string) bool {
	for _, candidate := range project.States {
		if candidate.Name == state {
			return candidate.Dispatchable && !candidate.Terminal && !candidate.OperatorOnly
		}
	}
	return false
}

func Issue(t *testing.T, repository tenantstore.Repository, details attachment.DetailStore, project tracker.NativeProject, options IssueOptions) SeededIssue {
	t.Helper()
	now := seedTime(options.Now)
	if len(project.States) == 0 {
		t.Fatal("seed issue: project has no workflow states")
	}
	if options.State == "" {
		options.State = project.States[0].Name
	}
	known := false
	terminal := false
	for _, state := range project.States {
		if state.Name == options.State {
			known, terminal = true, state.Terminal
		}
	}
	if !known {
		t.Fatalf("seed issue: state %q is not in the project workflow", options.State)
	}
	if options.Actor == (tracker.Actor{}) {
		options.Actor = tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	}
	id := tracker.NativeWorkItemID(hubapi.NewID("wi"))
	if options.Title == "" {
		options.Title = "Example issue " + string(id)
	}
	if options.Body == "" {
		options.Body = "Example body"
	}
	if options.Labels == nil {
		options.Labels = []string{}
	}
	if options.Assignees == nil {
		options.Assignees = []string{}
	}
	content := eventstore.NewContent(details)
	rawBody, err := json.Marshal(options.Body)
	if err != nil {
		t.Fatalf("seed issue: encode body: %v", err)
	}
	body, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: project.OrganizationID}, rawBody)
	if err != nil {
		t.Fatalf("seed issue: store body: %v", err)
	}
	seeded := SeededIssue{}
	write(t, repository, project.OrganizationID, "issue", func(w tenantstore.Writer) error {
		number, err := w.NextWorkItemNumber(t.Context(), project.ID)
		if err != nil {
			return err
		}
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: id, Number: int(number), Revision: 1, Profile: "native"}, Title: options.Title, Body: options.Body, State: options.State, Terminal: terminal, Priority: options.Priority, Actor: options.Actor, Labels: options.Labels, Assignees: options.Assignees, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
		if terminal {
			issue.ClosedAt = &now
		}
		rawIssue, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: id, RecordID: string(id), Revision: 1}, rawIssue)
		if err != nil {
			return err
		}
		rawEvent, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: project.OrganizationID, ProjectID: project.ID, AggregateID: id, Type: "issue.created", SchemaVersion: 1, Actor: options.Actor, RecordedAt: now, Data: tracker.CollaborationData{Revision: 1}})
		if err != nil {
			return err
		}
		event, err := content.PrepareEvent(t.Context(), project.OrganizationID, rawEvent)
		if err != nil {
			return err
		}
		facts := claimstore.WorkItemFacts{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: id, Author: options.Actor.PrincipalID, Assignees: options.Assignees}
		if dispatchable(project, options.State) {
			facts.DispatchWaitSince = &now
		}
		queue := claimstore.QueueEntry{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: id, Scope: string(project.ID), State: options.State, Rank: string(id), CreatedAt: now, UpdatedAt: now}
		if options.Priority != nil {
			queue.Priority = new(int64(*options.Priority))
		}
		row, err := w.CreateWorkItemWithClaimFacts(t.Context(), corestore.WorkItem{OrganizationID: project.OrganizationID, ProjectID: project.ID, NativeID: id, Number: number, Revision: 1, Title: options.Title, Priority: options.Priority, BodyObjectKey: &body.PayloadObjectKey, WorkflowStateName: &options.State, ActorKind: &options.Actor.Kind, ActorPrincipalID: &options.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}, options.Labels, facts, queue)
		if err != nil {
			return err
		}
		stored, err := w.CreateCollaborationVersion(t.Context(), version)
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		seeded = SeededIssue{Issue: issue, Row: row, Version: stored}
		return nil
	})
	return seeded
}

type RunnerOptions struct {
	Organization     bool
	Projects         []tracker.ProjectID
	Operations       []string
	DisplayName      string
	Hostname         string
	Capacity         int
	WithoutCapacity  bool
	CredentialExpiry time.Duration
	Now              time.Time
}

type SeededRunner struct {
	Identity       runnerauth.Identity
	Record         runnerstore.Identity
	Credential     SeededCredential
	CapacityConfig *runnerauth.CapacityConfig
}

func RunnerOperations() []string {
	return []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
}

func Runner(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, options RunnerOptions) SeededRunner {
	t.Helper()
	now := seedTime(options.Now)
	scope := "projects"
	if options.Organization {
		if len(options.Projects) != 0 {
			t.Fatal("seed runner: an organization runner takes no selected projects")
		}
		scope = "organization"
	} else if len(options.Projects) == 0 {
		t.Fatal("seed runner: a selected-project runner needs at least one project")
	}
	if options.Operations == nil {
		options.Operations = RunnerOperations()
	}
	if options.DisplayName == "" {
		options.DisplayName = "Example runner"
	}
	if options.Hostname == "" {
		options.Hostname = "example-host"
	}
	if options.Capacity == 0 {
		options.Capacity = 1
	}
	if options.CredentialExpiry == 0 {
		options.CredentialExpiry = runnerauth.CredentialTTL
	}
	binding := runnerauth.NewBinding()
	token := hubapi.NewID("token")
	expires := now.Add(options.CredentialExpiry)
	credential := SeededCredential{Organization: organization, ID: binding.RunnerID, Token: token, Hash: apikey.HashToken(token), Kind: "service"}
	enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: hubapi.NewID("enrollment"), RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken(hubapi.NewID("enrollment_token")), Scope: scope, ProjectIDs: append([]tracker.ProjectID(nil), options.Projects...), Operations: append([]string(nil), options.Operations...), CreatedAt: now, ExpiresAt: expires, CreatedBy: "fixture"}
	record := runnerstore.Identity{OrganizationID: organization, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: credential.ID, EnrollmentID: enrollment.ID, Scope: scope, State: "active", Operations: append([]string(nil), options.Operations...), DisplayName: options.DisplayName, CapacityLimit: int64(options.Capacity), ReportedCapacity: int64(options.Capacity), ProjectIDs: append([]tracker.ProjectID(nil), options.Projects...), Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}
	var config *runnerauth.CapacityConfig
	if !options.WithoutCapacity {
		config = &runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: options.Capacity, ClientLimit: options.Capacity, RuntimeLimit: options.Capacity, ObservedAt: now}
	}
	var stored runnerstore.Identity
	write(t, repository, organization, "runner", func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credential.ID, Name: credential.ID, Hash: credential.Hash, Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(binding.MachineID), Hostname: options.Hostname, DisplayName: options.DisplayName, Capacity: int64(options.Capacity), Version: "example"}); err != nil {
			return err
		}
		for _, project := range options.Projects {
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credential.ID, ProjectID: project}); err != nil {
				return err
			}
		}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, binding.RunnerID, binding.MachineID, now); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), record); err != nil {
			return err
		}
		if config != nil {
			if err := w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, *config); err != nil {
				return err
			}
		}
		var err error
		stored, err = w.RunnerIdentity(t.Context(), binding.RunnerID)
		return err
	})
	identity := runnerauth.Identity{Binding: binding, Scope: scope, OrganizationID: organization, ProjectIDs: append([]tracker.ProjectID(nil), options.Projects...), Operations: append([]string(nil), options.Operations...), ExpiresAt: expires}
	return SeededRunner{Identity: identity, Record: stored, Credential: credential, CapacityConfig: config}
}

type LeaseOptions struct {
	TTL       time.Duration
	SessionID string
	PolicyID  string
	Now       time.Time
}

func Lease(t *testing.T, repository tenantstore.Repository, runner SeededRunner, issue SeededIssue, options LeaseOptions) tracker.NativeLease {
	t.Helper()
	now := seedTime(options.Now)
	if options.TTL == 0 {
		options.TTL = time.Hour
	}
	if options.SessionID == "" {
		options.SessionID = hubapi.NewID("execution")
	}
	organization := issue.Row.OrganizationID
	var lease leasestore.Lease
	write(t, repository, organization, "lease", func(w tenantstore.Writer) error {
		var err error
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: issue.Row.ID, ExpectedRevision: issue.Row.Revision, MachineID: string(runner.Identity.MachineID), SessionID: options.SessionID, LeaseID: hubapi.NewID("lease"), TTL: options.TTL, RunnerID: runner.Identity.RunnerID})
		if err != nil {
			return err
		}
		if options.PolicyID == "" {
			return nil
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: issue.Row.ProjectID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: options.PolicyID, BoundAt: now})
	})
	return tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: issue.Row.NativeID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: options.PolicyID, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}
}

type ChangeOptions struct {
	Title string
	Input *tracker.ChangeVersionInput
	Actor tracker.Actor
	Now   time.Time
}

type SeededChange struct {
	Change     tracker.ChangeRequest
	Version    tracker.ChangeVersion
	Row        eventstore.Change
	VersionRow eventstore.Version
}

func ChangeArtifact(kind string) tracker.ChangeArtifact {
	return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
}

func ChangeInput(policyID string) tracker.ChangeVersionInput {
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: ChangeArtifact("code"), Artifacts: []tracker.ChangeArtifact{ChangeArtifact("manifest")}, PolicyID: policyID}
}

func Change(t *testing.T, repository tenantstore.Repository, details attachment.DetailStore, issue SeededIssue, approved SeededPolicy, options ChangeOptions) SeededChange {
	t.Helper()
	now := seedTime(options.Now)
	organization, project, item := issue.Row.OrganizationID, issue.Row.ProjectID, issue.Row.NativeID
	if options.Title == "" {
		options.Title = issue.Row.Title
	}
	if options.Actor == (tracker.Actor{}) {
		options.Actor = issue.Issue.Actor
	}
	input := ChangeInput(approved.Descriptor.ID)
	if options.Input != nil {
		input = *options.Input
	}
	content := eventstore.NewContent(details)
	change := tracker.ChangeRequest{ID: hubapi.NewID("change"), OrganizationID: organization, ProjectID: project, WorkItemID: item, LinkedIssues: []tracker.NativeWorkItemID{item}, Title: options.Title, Revision: 1, CreatedAt: now, UpdatedAt: now}
	version := tracker.ChangeVersion{ID: hubapi.NewID("version"), ChangeID: change.ID, Number: 1, ChangeVersionInput: input, Policy: approved.Descriptor, Actor: options.Actor, CreatedAt: now}
	prepareChange := func(value tracker.ChangeRequest) eventstore.Change {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("seed change: encode: %v", err)
		}
		row, err := content.PrepareChange(t.Context(), organization, raw)
		if err != nil {
			t.Fatalf("seed change: prepare: %v", err)
		}
		return row
	}
	created := prepareChange(change)
	rawVersion, err := json.Marshal(version)
	if err != nil {
		t.Fatalf("seed change: encode version: %v", err)
	}
	versionRow, err := content.PrepareVersion(t.Context(), organization, rawVersion)
	if err != nil {
		t.Fatalf("seed change: prepare version: %v", err)
	}
	published := change
	published.CurrentVersion = version.ID
	current := prepareChange(published)
	seeded := SeededChange{}
	write(t, repository, organization, "change", func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), created); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: organization, ProjectID: project, WorkItemID: item, ChangeID: change.ID, CreatedAt: now}); err != nil {
			return err
		}
		storedVersion, err := w.CreateVersion(t.Context(), versionRow)
		if err != nil {
			return err
		}
		row, err := w.UpdateChange(t.Context(), current)
		if err != nil {
			return err
		}
		published.Revision = tracker.Revision(row.Revision)
		seeded = SeededChange{Change: published, Version: version, Row: row, VersionRow: storedVersion}
		return nil
	})
	return seeded
}
