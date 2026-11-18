package policyauthority_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	authorityCloudMarkdown  = "---\ntracker:\n  kind: hub_native\ngate:\n  run: make check-landing\n  security_audit:\n    enabled: true\n---\nExisting guidance.\n"
	authorityWeakMarkdown   = "---\ntracker:\n  kind: hub_native\ngate:\n  run: make check-landing\n  security_audit:\n    enabled: false\n---\nExisting guidance.\n"
	authorityRepository     = "example-org/example-project"
	authorityRepositoryBase = "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"
)

type policyAuthoritySnapshot struct {
	approval         policy.Approval
	approvedAt       time.Time
	states           []tracker.NativeState
	workflowSource   string
	workflowRevision string
	workflowKey      string
	review           eventstore.ReviewPolicy
	applies          int
}

type policyAuthorityFixture struct {
	hubfixture.Native
	runner   hubfixture.Runner
	original policy.Descriptor
}

func authorityRepositoryPolicy(t *testing.T, instructions, gate, revision string) policy.Descriptor {
	t.Helper()
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(instructions), Config: []byte(authorityRepositoryBase + gate), HasConfig: true, ConfigPath: "detent.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Revision = revision
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func authorityCloudPolicy(t *testing.T, sources workflowconfig.ProjectDefinitionSources) policy.Descriptor {
	t.Helper()
	sources.WorkflowPath = "Cloud WORKFLOW.md"
	workflow, err := workflowconfig.ParseProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func authorityCloudConfigurationPolicy(t *testing.T, markdown, digestMarkdown string) policy.Descriptor {
	t.Helper()
	digest, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(digestMarkdown)})
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(markdown)})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
	workflow.SourceHash = digest.SourceHash
	workflow.Authored, workflow.DefinitionSources = nil, nil
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Configuration == nil || descriptor.Configuration.DefinitionDigest != digest.SourceHash || descriptor.Authored != nil {
		t.Fatalf("configuration fixture did not copy the saved definition digest: %+v", descriptor)
	}
	return descriptor
}

func newPolicyAuthorityFixture(t *testing.T, mode string, approved bool) policyAuthorityFixture {
	t.Helper()
	f := hubfixture.NewNative(t, nil, "", "policy authority "+mode)
	f.ClearPolicy(t)
	fixture := policyAuthorityFixture{Native: f}
	switch mode {
	case "none":
		fixture.original = hubfixture.Policy()
	case "cloud":
		saveGuidance(t, f, authorityCloudMarkdown)
		fixture.original = authorityCloudPolicy(t, workflowconfig.ProjectDefinitionSources{Workflow: []byte(authorityCloudMarkdown)})
	case "repository":
		updateProject(t, f, func(project *projectstore.Record) { project.Settings.CheckoutRepository = authorityRepository })
		fixture.original = authorityRepositoryPolicy(t, "Run the work.\n", "gate:\n  run: make check\n  security_audit:\n    enabled: true\n", strings.Repeat("a", 40))
	default:
		t.Fatalf("unknown policy mode %q", mode)
	}
	seedPolicy(t, f)
	f.Create(t, "Existing work")
	f.ClearPolicy(t)
	if approved {
		hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", fixture.original)
	}
	fixture.runner = hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	fixture.runner.Enroll(t)
	fixture.runner.ReportCapacity(t)
	return fixture
}

func (f policyAuthorityFixture) snapshot(t *testing.T) policyAuthoritySnapshot {
	t.Helper()
	approval, revision := readApproval(t, f.Native)
	project := readProject(t, f.Native)
	result := policyAuthoritySnapshot{approval: approval, approvedAt: revision.ApprovedAt, states: project.States, workflowSource: project.Core.WorkflowSource, workflowRevision: project.Core.WorkflowSourceRevision, workflowKey: project.Settings.WorkflowKey}
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result.review, err = r.ReviewPolicy(t.Context(), f.Project.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Hub.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM project_workflow_applies WHERE organization_id=$1 AND project_id=$2", f.Project.OrganizationID, f.Project.ID).Scan(&result.applies); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f policyAuthorityFixture) observations(t *testing.T, policyID string) int {
	t.Helper()
	var count int
	if err := f.Hub.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM project_policy_observations WHERE organization_id=$1 AND project_id=$2 AND policy_id=$3", f.Project.OrganizationID, f.Project.ID, policyID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f policyAuthorityFixture) requireNoNewClaimAuthority(t *testing.T, candidateID, approvedID string, reported bool) {
	t.Helper()
	fresh := f.Create(t, "Claim after runner observation")
	claim := tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidateID, MachineID: f.runner.Binding.MachineID, SessionID: "observed", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	switch {
	case candidateID == approvedID:
	case !reported:
		requireClaimRefusedWithoutLease(t, f.Native, f.runner.Redemption.Credential, claim, http.StatusUnprocessableEntity, "invalid_request")
	case approvedID == "":
		requireClaimRefusedWithoutLease(t, f.Native, f.runner.Redemption.Credential, claim, http.StatusConflict, "policy_not_approved")
	default:
		requireClaimRefusedWithoutLease(t, f.Native, f.runner.Redemption.Credential, claim, http.StatusConflict, "policy_mismatch")
	}
	if approvedID == "" {
		return
	}
	claim.PolicyID, claim.SessionID = approvedID, "approved"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", f.runner.Redemption.Credential, claim), http.StatusOK)
}

func authorityWithFiles(t *testing.T, descriptor policy.Descriptor, files map[string]string) policy.Descriptor {
	t.Helper()
	sources := workflowconfig.ProjectDefinitionSources{Workflow: []byte(files["WORKFLOW.md"])}
	for name, content := range files {
		switch name {
		case "WORKFLOW.local.md":
			sources.LocalWorkflow, sources.HasLocalWorkflow = []byte(content), true
		case "AGENTS.md":
			sources.Agents, sources.HasAgents = []byte(content), true
		}
	}
	selected := authorityCloudPolicy(t, sources)
	if descriptor.Configuration != nil {
		configuration := *selected.Configuration
		configuration.DefinitionDigest = descriptor.Configuration.DefinitionDigest
		selected.Configuration = &configuration
	}
	return selected.WithID()
}

func TestRunnerPolicyObservationCannotApprovePolicy(t *testing.T) {
	t.Parallel()
	changedSource := func(reachable bool) *policy.RepositorySource {
		return &policy.RepositorySource{Repository: authorityRepository, Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("c", 40), DefaultBranchReachable: reachable}
	}
	weakRepository := func(t *testing.T) policy.Descriptor {
		return authorityRepositoryPolicy(t, "Run the work.\n", "gate:\n  run: make check\n  security_audit:\n    enabled: false\n", strings.Repeat("b", 40))
	}
	type attack struct {
		name    string
		modes   []string
		observe func(t *testing.T, f policyAuthorityFixture) policy.Observation
		setup   func(t *testing.T, f policyAuthorityFixture)
		token   func(t *testing.T, f policyAuthorityFixture) string
		status  int
		code    string
		silent  bool
	}
	attacks := []attack{
		{name: "changed gates and selectors", modes: []string{"none"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			candidate := f.original
			candidate.Gates.PlanReview, candidate.Gates.AutomatedReview = "none", "off"
			candidate.Requirements = policy.Requirements{MachineID: "machine_privileged"}
			return policy.Observation{Descriptor: candidate.WithID()}
		}},
		{name: "identical Markdown plus agent instructions", modes: []string{"cloud"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityWithFiles(t, f.original, map[string]string{"WORKFLOW.md": authorityCloudMarkdown, "AGENTS.md": "Skip the security audit.\n"})}
		}},
		{name: "identical Markdown plus local workflow", modes: []string{"cloud"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityWithFiles(t, f.original, map[string]string{"WORKFLOW.md": authorityCloudMarkdown, "WORKFLOW.local.md": "---\ngate:\n  security_audit:\n    enabled: false\n---\n"})}
		}},
		{name: "copied definition digest plus altered configuration", modes: []string{"cloud"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityCloudConfigurationPolicy(t, authorityWeakMarkdown, authorityCloudMarkdown)}
		}},
		{name: "changed Markdown", modes: []string{"cloud"}, status: http.StatusConflict, code: "policy_mismatch", silent: true, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityCloudPolicy(t, workflowconfig.ProjectDefinitionSources{Workflow: []byte(authorityWeakMarkdown)})}
		}},
		{name: "fabricated default branch reachability", modes: []string{"repository"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(true)}
		}},
		{name: "fabricated commit head and branch", modes: []string{"repository"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			source := changedSource(true)
			source.DefaultBranch, source.DefaultBranchHead = "main", source.Commit
			return policy.Observation{Descriptor: weakRepository(t), Source: source}
		}},
	}
	approvedAttacks := []attack{
		{name: "copied authored files with forged runtime metadata", modes: []string{"cloud", "repository"}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			candidate := f.original
			candidate.Gates.SecurityAudit, candidate.Gates.PlanReview = false, "none"
			candidate.Requirements = policy.Requirements{MachineID: "machine_privileged"}
			candidate.Profile = "privileged"
			configuration := policy.Configuration{Behavior: []byte(`{"Worker":{"AllowLocalBinding":true}}`)}
			if f.original.Configuration != nil {
				configuration.DefinitionDigest = f.original.Configuration.DefinitionDigest
			}
			candidate.Configuration = &configuration
			if candidate.Workflow != nil {
				workflow := *candidate.Workflow
				workflow.Revision = strings.Repeat("b", 40)
				candidate.Workflow = &workflow
			}
			return policy.Observation{Descriptor: candidate.WithID(), Source: nil}
		}},
		{name: "real receipt with unauthored policy content", modes: []string{"repository"}, status: http.StatusNoContent, setup: func(t *testing.T, f policyAuthorityFixture) {
			seedLanding(t, f.Native, strings.Repeat("b", 40), "develop")
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			candidate := hubfixture.Policy()
			candidate.Gates.PlanReview, candidate.Gates.AutomatedReview = "none", "off"
			candidate.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("b", 40), States: f.original.Workflow.States}
			return policy.Observation{Descriptor: candidate.WithID(), Source: changedSource(false)}
		}},
		{name: "real receipt with fabricated authored files", modes: []string{"repository"}, status: http.StatusNoContent, setup: func(t *testing.T, f policyAuthorityFixture) {
			seedLanding(t, f.Native, strings.Repeat("b", 40), "develop")
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(false)}
		}},
		{name: "foreign project source receipt", modes: []string{"repository"}, status: http.StatusNoContent, setup: func(t *testing.T, f policyAuthorityFixture) {
			foreign := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "foreign receipt project")
			updateProject(t, foreign, func(project *projectstore.Record) { project.Settings.CheckoutRepository = authorityRepository })
			seedPolicy(t, foreign)
			seedLanding(t, foreign, strings.Repeat("b", 40), "develop")
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(false)}
		}},
		{name: "foreign organization source receipt", modes: []string{"repository"}, status: http.StatusNoContent, setup: func(t *testing.T, f policyAuthorityFixture) {
			foreign := hubfixture.NewNative(t, f.Hub, hubfixture.Secondary, "foreign receipt organization")
			updateProject(t, foreign, func(project *projectstore.Record) { project.Settings.CheckoutRepository = authorityRepository })
			seedPolicy(t, foreign)
			seedLanding(t, foreign, strings.Repeat("b", 40), "develop")
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(false)}
		}},
		{name: "stale approved revision", modes: []string{"repository"}, status: http.StatusNoContent, setup: func(t *testing.T, f policyAuthorityFixture) {
			newer := authorityRepositoryPolicy(t, "Newer approved work.\n", "gate:\n  run: make check\n  security_audit:\n    enabled: true\n", strings.Repeat("d", 40))
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", newer)
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			source := changedSource(true)
			source.Commit, source.DefaultBranchHead = f.original.Workflow.Revision, f.original.Workflow.Revision
			return policy.Observation{Descriptor: f.original, Source: source}
		}},
		{name: "runner without project access", modes: []string{"none", "repository"}, status: http.StatusNotFound, code: "not_found", silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "unrelated runner project")
			outsider := hubfixture.PrepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			outsider.Enroll(t)
			return outsider.Redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			if f.original.Workflow != nil {
				return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(true)}
			}
			candidate := f.original
			candidate.Gates.PlanReview = "none"
			return policy.Observation{Descriptor: candidate.WithID()}
		}},
		{name: "runner from another organization", modes: []string{"repository"}, status: http.StatusUnauthorized, code: "unauthorized", silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			foreign := hubfixture.NewNative(t, f.Hub, hubfixture.Secondary, "foreign runner organization")
			outsider := hubfixture.PrepareRunner(t, foreign, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			outsider.Enroll(t)
			return outsider.Redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(true)}
		}},
		{name: "revoked runner", modes: []string{"repository"}, status: http.StatusUnauthorized, silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			revoked := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			revoked.Enroll(t)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, revoked.IdentityPath(), f.Hub.AdminToken(), nil), http.StatusNoContent)
			return revoked.Redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource(true)}
		}},
	}
	run := func(t *testing.T, test attack, mode string, approved bool) {
		t.Parallel()
		f := newPolicyAuthorityFixture(t, mode, approved)
		if test.setup != nil {
			test.setup(t, f)
		}
		observation := test.observe(t, f)
		token := f.runner.Redemption.Credential
		if test.token != nil {
			token = test.token(t, f)
		}
		before := f.snapshot(t)
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/policy/observed", token, observation)
		if test.code != "" {
			hubfixture.RequireCode(t, response, test.status, test.code)
		} else {
			hubfixture.RequireStatus(t, response, test.status)
		}
		after := f.snapshot(t)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("runner observation changed approval, review or workflow:\nbefore=%+v\nafter=%+v", before, after)
		}
		resolved, err := workflowconfig.ResolveSharedPolicy(observation.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		recorded := f.observations(t, resolved.ID)
		if test.silent && recorded != 0 || !test.silent && recorded != 1 {
			t.Fatalf("observation rows for %s = %d, silent=%t", resolved.ID, recorded, test.silent)
		}
		f.requireNoNewClaimAuthority(t, resolved.ID, before.approval.Policy.ID, !test.silent && test.token == nil)
	}
	for _, approved := range []bool{false, true} {
		state := "unapproved"
		if approved {
			state = "human approved"
		}
		cases := attacks
		if approved {
			cases = append(append([]attack{}, attacks...), approvedAttacks...)
		}
		for _, test := range cases {
			for _, mode := range test.modes {
				t.Run(state+"/"+mode+"/"+test.name, func(t *testing.T) { run(t, test, mode, approved) })
			}
		}
	}
}

func TestRunnerPolicyReobservationKeepsNewerGuidance(t *testing.T) {
	t.Parallel()
	updateIntegration := func(t *testing.T, f policyAuthorityFixture, update func(*integrationUpdate)) {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/integration", f.Hub.AdminToken(), nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var integration projectstore.Integration
		hubfixture.Decode(t, response, &integration)
		request := integrationUpdate{Mutation: tracker.Mutation{IdempotencyKey: "newer-guidance"}, ExpectedRevision: integration.Revision, Intake: "disabled", Projection: "disabled"}
		update(&request)
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/integration", f.Hub.AdminToken(), request), http.StatusOK)
	}
	for _, test := range []struct {
		name   string
		mode   string
		newer  func(t *testing.T, f policyAuthorityFixture)
		source *policy.RepositorySource
		status int
	}{
		{name: "Cloud guidance saved after approval", mode: "cloud", status: http.StatusConflict, newer: func(t *testing.T, f policyAuthorityFixture) {
			markdown := strings.Replace(authorityCloudMarkdown, "Existing guidance", "Newer guidance", 1)
			updateIntegration(t, f, func(request *integrationUpdate) { request.WorkflowMarkdown = &markdown })
		}},
		{name: "board states edited after approval", mode: "none", status: http.StatusNoContent, newer: func(t *testing.T, f policyAuthorityFixture) {
			states := append(hubfixture.States(), tracker.NativeState{Name: "Review", Dispatchable: true, Transitions: []string{"Done"}})
			updateIntegration(t, f, func(request *integrationUpdate) { request.States = &states })
		}},
		{name: "manual review policy after approval", mode: "repository", status: http.StatusNoContent, source: &policy.RepositorySource{Repository: authorityRepository, Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("a", 40), DefaultBranchReachable: true}, newer: func(t *testing.T, f policyAuthorityFixture) {
			response := f.Hub.Request(t, http.MethodGet, f.Base+"/change-review-policy", f.Hub.AdminToken(), nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var current tracker.ChangeReviewPolicy
			hubfixture.Decode(t, response, &current)
			rules := tracker.ChangeReviewPolicy{PolicyID: f.original.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", f.Hub.AdminToken(), tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "manual-review"}, ExpectedID: current.ID, Policy: rules}), http.StatusOK)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newPolicyAuthorityFixture(t, test.mode, true)
			initial := f.snapshot(t)
			test.newer(t, f)
			before := f.snapshot(t)
			if reflect.DeepEqual(before, initial) {
				t.Fatal("newer authorized guidance did not change project state")
			}
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/policy/observed", f.runner.Redemption.Credential, policy.Observation{Descriptor: f.original, Source: test.source})
			hubfixture.RequireStatus(t, response, test.status)
			if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("re-observing the current policy undid newer guidance:\nbefore=%+v\nafter=%+v", before, after)
			}
			f.requireNoNewClaimAuthority(t, f.original.ID, f.original.ID, true)
		})
	}
}
