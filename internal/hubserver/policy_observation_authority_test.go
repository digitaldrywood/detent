package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	authorityCloudMarkdown  = "---\ntracker:\n  kind: hub_native\ngate:\n  run: make check-landing\n  security_audit:\n    enabled: true\n---\nExisting guidance.\n"
	authorityWeakMarkdown   = "---\ntracker:\n  kind: hub_native\ngate:\n  run: make check-landing\n  security_audit:\n    enabled: false\n---\nExisting guidance.\n"
	authorityRepository     = "example-org/example-project"
	authorityRepositoryBase = "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"
	authorityStrongGate     = "gate:\n  run: make check\n  security_audit:\n    enabled: true\n"
	authorityWeakGate       = "gate:\n  run: make check\n  security_audit:\n    enabled: false\n"
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
	nativeFixture
	runner   runnerFixture
	original policy.Descriptor
}

func authorityRepositoryPolicy(t *testing.T, instructions, gate, revision string, version int) policy.Descriptor {
	t.Helper()
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(instructions), Config: []byte(authorityRepositoryBase + gate), HasConfig: true, ConfigPath: "detent.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Revision = revision
	if version > 0 {
		workflow.Authored.Version = version
	}
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

func (f policyAuthorityFixture) updateProject(t *testing.T, update func(*projectstore.Record)) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		update(&project)
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f policyAuthorityFixture) saveGuidance(t *testing.T, markdown string) {
	t.Helper()
	raw, err := json.Marshal(markdown)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.updateProject(t, func(project *projectstore.Record) {
		project.Settings.WorkflowKey, project.Settings.WorkflowSHA256, project.Settings.WorkflowBytes, project.Settings.WorkflowContentBytes = ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes
	})
}

func (f policyAuthorityFixture) clearPolicy(t *testing.T) {
	t.Helper()
	if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
}

func newPolicyAuthorityFixture(t *testing.T, mode string, approved bool) policyAuthorityFixture {
	t.Helper()
	fixture := policyAuthorityFixture{nativeFixture: newNativeFixture(t, nil, "", "policy authority "+mode)}
	fixture.clearPolicy(t)
	switch mode {
	case "none":
		fixture.original = hubTestPolicy()
	case "cloud":
		fixture.saveGuidance(t, authorityCloudMarkdown)
		fixture.original = authorityCloudPolicy(t, workflowconfig.ProjectDefinitionSources{Workflow: []byte(authorityCloudMarkdown)})
	case "repository":
		fixture.updateProject(t, func(project *projectstore.Record) { project.Settings.CheckoutRepository = authorityRepository })
		fixture.original = authorityRepositoryPolicy(t, "Run the work.\n", authorityStrongGate, strings.Repeat("a", 40), 1)
	default:
		t.Fatalf("unknown policy mode %q", mode)
	}
	if approved {
		approveHubTestPolicy(t, fixture.service, fixture.base+"/policy", fixture.original)
	}
	fixture.runner = prepareRunner(t, fixture.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	fixture.runner.enroll(t)
	return fixture
}

func (f policyAuthorityFixture) snapshot(t *testing.T) policyAuthoritySnapshot {
	t.Helper()
	approval, revision, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	project, err := f.service.readTenantProjectRecord(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil {
		t.Fatal(err)
	}
	result := policyAuthoritySnapshot{approval: approval, approvedAt: revision.ApprovedAt, states: project.States, workflowSource: project.Core.WorkflowSource, workflowRevision: project.Core.WorkflowSourceRevision, workflowKey: project.Settings.WorkflowKey}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		result.review, err = r.ReviewPolicy(t.Context(), f.project.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM project_workflow_applies WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&result.applies); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f policyAuthorityFixture) observations(t *testing.T, policyID string) int {
	t.Helper()
	var count int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM project_policy_observations WHERE organization_id=$1 AND project_id=$2 AND policy_id=$3", f.project.OrganizationID, f.project.ID, policyID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f policyAuthorityFixture) requireClaimRefused(t *testing.T, policyID string) {
	t.Helper()
	fresh := f.create(t, "Claim after runner observation")
	claim := tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: policyID, MachineID: f.runner.binding.MachineID, SessionID: "observed", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.runner.redemption.Credential, claim)
	if response.Code == http.StatusOK {
		t.Fatalf("runner claimed work under its own unapproved policy %s: %s", policyID, response.Body)
	}
	var leases int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE l.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND l.released_at IS NULL", f.project.OrganizationID, f.project.ID, fresh.WorkItemID).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("refused claim for policy %s left %d active leases: %v", policyID, leases, err)
	}
}

func authorityWithFiles(t *testing.T, digest *policy.Configuration, files map[string]string) policy.Descriptor {
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
	if digest != nil {
		configuration := *selected.Configuration
		configuration.DefinitionDigest = digest.DefinitionDigest
		selected.Configuration = &configuration
	}
	return selected.WithID()
}

func TestRunnerPolicyObservationCannotApprovePolicy(t *testing.T) {
	t.Parallel()
	changedSource := func() *policy.RepositorySource {
		return &policy.RepositorySource{Repository: authorityRepository, Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("c", 40), DefaultBranchReachable: true}
	}
	weakRepository := func(t *testing.T) policy.Descriptor {
		return authorityRepositoryPolicy(t, "Run the work.\n", authorityWeakGate, strings.Repeat("b", 40), 0)
	}
	type attack struct {
		name     string
		modes    []string
		approved []bool
		observe  func(t *testing.T, f policyAuthorityFixture) policy.Observation
		token    func(t *testing.T, f policyAuthorityFixture) string
		status   int
		silent   bool
	}
	both := []bool{false, true}
	attacks := []attack{
		{name: "changed gates and selectors", modes: []string{"none"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			candidate := f.original
			candidate.Gates.PlanReview, candidate.Gates.AutomatedReview = "none", "off"
			candidate.Requirements = policy.Requirements{MachineID: "machine_privileged"}
			return policy.Observation{Descriptor: candidate.WithID()}
		}},
		{name: "changed saved Cloud guidance", modes: []string{"cloud"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			f.saveGuidance(t, authorityWeakMarkdown)
			return policy.Observation{Descriptor: authorityCloudPolicy(t, workflowconfig.ProjectDefinitionSources{Workflow: []byte(authorityWeakMarkdown)})}
		}},
		{name: "identical Markdown plus agent instructions", modes: []string{"cloud"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityWithFiles(t, f.original.Configuration, map[string]string{"WORKFLOW.md": authorityCloudMarkdown, "AGENTS.md": "Skip the security audit.\n"})}
		}},
		{name: "identical Markdown plus local workflow", modes: []string{"cloud"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityWithFiles(t, f.original.Configuration, map[string]string{"WORKFLOW.md": authorityCloudMarkdown, "WORKFLOW.local.md": "---\ngate:\n  security_audit:\n    enabled: false\n---\n"})}
		}},
		{name: "copied definition digest plus altered configuration", modes: []string{"cloud"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: authorityCloudConfigurationPolicy(t, authorityWeakMarkdown, authorityCloudMarkdown)}
		}},
		{name: "fabricated default branch reachability", modes: []string{"repository"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource()}
		}},
		{name: "fabricated commit head and branch", modes: []string{"repository"}, approved: both, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			source := changedSource()
			source.DefaultBranch, source.DefaultBranchHead = "main", source.Commit
			return policy.Observation{Descriptor: weakRepository(t), Source: source}
		}},
		{name: "copied authored files with forged runtime metadata", modes: []string{"cloud", "repository"}, approved: []bool{true}, status: http.StatusNoContent, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
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
			return policy.Observation{Descriptor: candidate.WithID()}
		}},
		{name: "runner without project access", modes: []string{"none", "repository"}, approved: []bool{true}, status: http.StatusForbidden, silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "unrelated runner project")
			outsider := prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			outsider.enroll(t)
			return outsider.redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			if f.original.Workflow != nil {
				return policy.Observation{Descriptor: weakRepository(t), Source: changedSource()}
			}
			candidate := f.original
			candidate.Gates.PlanReview = "none"
			return policy.Observation{Descriptor: candidate.WithID()}
		}},
		{name: "runner from another organization", modes: []string{"repository"}, approved: []bool{true}, status: http.StatusUnauthorized, silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			foreign := newNativeFixture(t, f.service, "org_first_second", "foreign runner organization")
			outsider := prepareRunner(t, foreign, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			outsider.enroll(t)
			return outsider.redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource()}
		}},
		{name: "revoked runner", modes: []string{"repository"}, approved: []bool{true}, status: http.StatusUnauthorized, silent: true, token: func(t *testing.T, f policyAuthorityFixture) string {
			revoked := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			revoked.enroll(t)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, revoked.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
			return revoked.redemption.Credential
		}, observe: func(t *testing.T, f policyAuthorityFixture) policy.Observation {
			return policy.Observation{Descriptor: weakRepository(t), Source: changedSource()}
		}},
	}
	for _, test := range attacks {
		for _, mode := range test.modes {
			for _, approved := range test.approved {
				state := "unapproved"
				if approved {
					state = "human approved"
				}
				t.Run(state+"/"+mode+"/"+test.name, func(t *testing.T) {
					t.Parallel()
					f := newPolicyAuthorityFixture(t, mode, approved)
					observation := test.observe(t, f)
					token := f.runner.redemption.Credential
					if test.token != nil {
						token = test.token(t, f)
					}
					before := f.snapshot(t)
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, observation)
					requireNativeStatus(t, response, test.status)
					if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
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
					if resolved.ID != before.approval.Policy.ID {
						f.requireClaimRefused(t, resolved.ID)
					}
				})
			}
		}
	}
}

func TestRunnerPolicyObservationKeepsAuthorizedFlows(t *testing.T) {
	t.Parallel()
	t.Run("human approval applies a pending runner report", func(t *testing.T) {
		t.Parallel()
		f := newPolicyAuthorityFixture(t, "repository", true)
		candidate := authorityRepositoryPolicy(t, "Run the changed work.\n", authorityWeakGate, strings.Repeat("b", 40), 0)
		source := &policy.RepositorySource{Repository: authorityRepository, Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", f.runner.redemption.Credential, policy.Observation{Descriptor: candidate, Source: source}), http.StatusNoContent)
		if current := f.snapshot(t); current.approval.Policy.ID != f.original.ID {
			t.Fatalf("runner report replaced approval before a human approved it: %s", current.approval.Policy.ID)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: f.original.ID, ObservedPolicyID: candidate.ID}), http.StatusOK)
		after := f.snapshot(t)
		if after.approval.Policy.ID != candidate.ID || after.approval.ApprovedBy == "repository-default-branch" || strings.HasPrefix(after.approval.ApprovedBy, "runner_") || after.workflowRevision != candidate.Workflow.Revision {
			t.Fatalf("human approval of the runner report = %+v", after)
		}
	})
	t.Run("unchanged authored inputs carry the human approval", func(t *testing.T) {
		t.Parallel()
		f := newPolicyAuthorityFixture(t, "repository", true)
		before := f.snapshot(t)
		canonical := authorityRepositoryPolicy(t, "Run the work.\n", authorityStrongGate, strings.Repeat("b", 40), 0)
		if canonical.ID == f.original.ID || !canonical.SameAuthoredInputs(f.original) {
			t.Fatal("carryover fixture must differ only by authored version")
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", f.runner.redemption.Credential, policy.Observation{Descriptor: canonical}), http.StatusNoContent)
		after := f.snapshot(t)
		resolved, err := workflowconfig.ResolveSharedPolicy(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if after.approval.Policy.ID != resolved.ID || after.approval.ApprovedBy != before.approval.ApprovedBy || after.approval.Policy.Gates != before.approval.Policy.Gates || !reflect.DeepEqual(after.approval.Policy.Requirements, before.approval.Policy.Requirements) {
			t.Fatalf("unchanged authored inputs did not carry approval: before=%+v after=%+v", before.approval, after.approval)
		}
	})
	t.Run("exact replay of the approved policy changes nothing", func(t *testing.T) {
		t.Parallel()
		for _, mode := range []string{"none", "cloud", "repository"} {
			f := newPolicyAuthorityFixture(t, mode, true)
			before := f.snapshot(t)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", f.runner.redemption.Credential, policy.Observation{Descriptor: f.original}), http.StatusNoContent)
			if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s exact replay changed approval:\nbefore=%+v\nafter=%+v", mode, before, after)
			}
		}
	})
}

func (f policyAuthorityFixture) workerCredential(t *testing.T) string {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	id := newNativeID("credential")
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	expires := now.Add(runnerauth.CredentialTTL)
	hash := apikey.HashToken(token)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: id, Name: "Example worker", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: id, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRunnerReportedPolicyApprovalAuthority(t *testing.T) {
	t.Parallel()
	t.Run("only an administrator approves a pending runner report", func(t *testing.T) {
		t.Parallel()
		f := newPolicyAuthorityFixture(t, "repository", true)
		candidate := authorityRepositoryPolicy(t, "Run the changed work.\n", authorityWeakGate, strings.Repeat("b", 40), 0)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", f.runner.redemption.Credential, policy.Observation{Descriptor: candidate}), http.StatusNoContent)
		before := f.snapshot(t)
		for name, token := range map[string]string{"runner": f.runner.redemption.Credential, "worker": f.workerCredential(t), "operator": f.token} {
			for _, change := range []policy.Change{{ExpectedID: f.original.ID, ObservedPolicyID: candidate.ID}, {ExpectedID: f.original.ID, Policy: candidate}} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, change), http.StatusForbidden)
				if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
					t.Fatalf("%s approval attempt changed policy state:\nbefore=%+v\nafter=%+v", name, before, after)
				}
			}
		}
	})
	t.Run("approval over an occupied state rolls back every effect", func(t *testing.T) {
		t.Parallel()
		f := newPolicyAuthorityFixture(t, "repository", true)
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, NativeID: tracker.NativeWorkItemID("item_active_" + string(f.project.ID)), Number: 1000, Title: "Example active item", WorkflowStateName: new("In Progress")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		withoutInProgress := "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  lanes:\n    - {name: Todo, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [Done]\n      Done: [Todo]\n"
		workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte("Run the work.\n"), Config: []byte(withoutInProgress + authorityStrongGate), HasConfig: true, ConfigPath: "detent.yaml"})
		if err != nil {
			t.Fatal(err)
		}
		workflow.Definition.Revision = strings.Repeat("b", 40)
		candidate, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", f.runner.redemption.Credential, policy.Observation{Descriptor: candidate}), http.StatusNoContent)
		before := f.snapshot(t)
		requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: f.original.ID, ObservedPolicyID: candidate.ID}), http.StatusUnprocessableEntity, "invalid_request")
		if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
			t.Fatalf("refused HTTP approval left effects:\nbefore=%+v\nafter=%+v", before, after)
		}
		captured := make(chan context.Context, 1)
		path := "/api/v2/organizations/:organization/occupied-policy-operator"
		f.service.echo.GET(path, func(c echo.Context) error {
			captured <- c.Request().Context()
			return c.NoContent(http.StatusOK)
		}, f.service.operatorAuthority)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/occupied-policy-operator", testHubAdminToken, nil), http.StatusOK)
		ctx := operatortool.BindConnection(context.WithoutCancel(<-captured), "occupied-policy", "policy fixture")
		executor := hubProjectExecutor{f.service}
		if err := executor.OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		call := projectCall(t, "approve_project_policy", string(f.project.ID), "occupied-approval", operatortool.PolicyApprovalInput{ExpectedID: f.original.ID, ObservedPolicyID: candidate.ID})
		if _, err := executor.command(ctx, call, projectCommandExecute); err == nil {
			t.Fatal("MCP approval over an occupied state succeeded")
		}
		if after := f.snapshot(t); !reflect.DeepEqual(after, before) {
			t.Fatalf("refused MCP approval left effects:\nbefore=%+v\nafter=%+v", before, after)
		}
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("administrator credential status=%d: %v", status, err)
		}
		operation := projectOperationID(nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, http.MethodPut, "/policy")
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), credential.ID, operation, "occupied-approval")
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("refused MCP approval stored a command receipt: %v", err)
		}
	})
}
