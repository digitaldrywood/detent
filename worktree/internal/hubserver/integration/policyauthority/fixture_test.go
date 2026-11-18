package policyauthority_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type integrationUpdate struct {
	tracker.Mutation
	States            *[]tracker.NativeState `json:"states,omitempty"`
	WorkflowMarkdown  *string                `json:"workflow_markdown,omitempty"`
	ExpectedRevision  tracker.Revision       `json:"expected_revision,string"`
	Intake            string                 `json:"intake"`
	Projection        string                 `json:"projection"`
	RepositoryEnabled bool                   `json:"repository_enabled"`
}

func updateProject(t *testing.T, p hubfixture.Native, update func(*projectstore.Record)) {
	t.Helper()
	if err := p.Hub.Repository.WriteTenant(t.Context(), p.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), p.Project.ID)
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

func saveGuidance(t *testing.T, p hubfixture.Native, markdown string) {
	t.Helper()
	raw, err := json.Marshal(markdown)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(p.Hub.Config.RunDetails).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: p.Project.OrganizationID}, raw)
	if err != nil {
		t.Fatal(err)
	}
	updateProject(t, p, func(project *projectstore.Record) {
		project.Settings.WorkflowKey, project.Settings.WorkflowSHA256, project.Settings.WorkflowBytes, project.Settings.WorkflowContentBytes = ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes
	})
}

func seedPolicy(t *testing.T, p hubfixture.Native) {
	t.Helper()
	hubtest.Policy(t, p.Hub.Repository, p.Hub.Config.RunDetails, p.Project, hubtest.PolicyOptions{Now: p.Hub.Now()})
}

func readApproval(t *testing.T, p hubfixture.Native) (policy.Approval, policystore.Revision) {
	t.Helper()
	var revision policystore.Revision
	err := p.Hub.Repository.ReadTenant(t.Context(), p.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		revision, err = r.ProjectPolicy(t.Context(), p.Project.ID)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return policy.Approval{}, policystore.Revision{}
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := eventstore.NewContent(p.Hub.Config.RunDetails).Receipt(t.Context(), eventstore.Receipt{OrganizationID: p.Project.OrganizationID, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256, PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := hubrecords.DecodeResolvedProjectPolicy(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return policy.Approval{Policy: resolved.Descriptor(), ApprovedBy: revision.ApprovedBy, ApprovedAt: hubapi.FormatHubTime(revision.ApprovedAt)}, revision
}

func readProject(t *testing.T, p hubfixture.Native) projectstore.Record {
	t.Helper()
	var project projectstore.Record
	if err := p.Hub.Repository.ReadTenant(t.Context(), p.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(t.Context(), p.Project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return project
}

func authenticate(t *testing.T, p hubfixture.Native, token string) hubscope.NativeScope {
	t.Helper()
	authenticator := hubauth.New(hubauth.Deps{Repository: p.Hub.Repository, AccessRepository: p.Hub.Repository, Organization: p.Hub.Primary, Now: p.Hub.Config.Clock, Classify: hubscope.OperatorAuthorizationError})
	credential, status, err := hubscope.NativeCredentialResult(authenticator.AuthenticateRepositoryAPIHash(t.Context(), p.Hub.Repository, p.Project.OrganizationID, apikey.HashToken(token), "", ""))
	if err != nil || status != http.StatusOK {
		t.Fatalf("credential status=%d: %v", status, err)
	}
	return hubscope.NativeScope{Organization: p.Project.OrganizationID, Project: p.Project.ID, Credential: credential}
}

func requireClaimRefusedWithoutLease(t *testing.T, p hubfixture.Native, token string, claim tracker.NativeClaim, status int, code string) {
	t.Helper()
	hubfixture.RequireCode(t, p.Hub.Request(t, http.MethodPost, p.Base+"/claims", token, claim), status, code)
	var leases int
	if err := p.Hub.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE l.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND l.released_at IS NULL", p.Project.OrganizationID, p.Project.ID, claim.WorkItemID).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("refused claim for policy %s left %d active leases: %v", claim.PolicyID, leases, err)
	}
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedLanding(t *testing.T, p hubfixture.Native, commit, baseRef string) {
	t.Helper()
	source := p.Create(t, "Landing provenance source")
	approval, _ := readApproval(t, p)
	runner := hubfixture.PrepareRunner(t, p, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.Enroll(t)
	runner.ReportCapacity(t)
	response := p.Hub.Request(t, http.MethodPost, p.Base+"/claims", runner.Redemption.Credential, tracker.NativeClaim{WorkItemID: source.WorkItemID, PolicyID: approval.Policy.ID, MachineID: runner.Binding.MachineID, SessionID: "landing-source", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	event := hubfixture.StartedEvent(lease)
	path := p.Base + "/work-items/" + string(source.WorkItemID) + "/events"
	hubfixture.RequireStatus(t, p.Hub.Request(t, http.MethodPost, path, runner.Redemption.Credential, event), http.StatusOK)
	actor := authenticate(t, p, runner.Redemption.Credential).Actor()
	now := p.Hub.Now().UTC().Truncate(time.Microsecond)
	change := tracker.ChangeRequest{ID: "change_landed", OrganizationID: p.Project.OrganizationID, ProjectID: p.Project.ID, WorkItemID: source.WorkItemID, Title: "Landed source", Revision: 1, CreatedAt: now, UpdatedAt: now}
	content := eventstore.NewContent(p.Hub.Config.RunDetails)
	changeRow, err := content.PrepareChange(t.Context(), p.Project.OrganizationID, marshal(t, change))
	if err != nil {
		t.Fatal(err)
	}
	input := hubfixture.ChangeInput()
	input.Repository = "https://github.com/example-org/example-project"
	input.HeadSHA = strings.Repeat("b", 40)
	input.PolicyID = approval.Policy.ID
	input.RunID, input.AttemptID = event.Data.RunID, event.Data.AttemptID
	version := tracker.ChangeVersion{ID: "version_landed", ChangeID: change.ID, Number: 1, ChangeVersionInput: input, Policy: approval.Policy, Actor: authenticate(t, p, p.Token).Actor(), CreatedAt: now}
	versionRow, err := content.PrepareVersion(t.Context(), p.Project.OrganizationID, marshal(t, version))
	if err != nil {
		t.Fatal(err)
	}
	change.CurrentVersion = version.ID
	change.Revision++
	change.Landed = &tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: commit, BaseRef: baseRef, Method: "merge", Actor: actor, LandedAt: now}
	updated, err := content.PrepareChange(t.Context(), p.Project.OrganizationID, marshal(t, change))
	if err != nil {
		t.Fatal(err)
	}
	updated.Revision = 1
	frozen, err := content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: p.Project.OrganizationID, ProjectID: p.Project.ID, WorkItemID: source.WorkItemID, ChangeID: change.ID, VersionID: version.ID}, *change.Landed)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: p.Project.OrganizationID, AttemptID: event.Data.AttemptID, VersionID: version.ID, Repository: version.Repository, BaseRef: baseRef}, tracker.NativeLandingReceipt{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: commit, BaseRef: baseRef, Method: "merge", Landed: true, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Hub.Repository.WriteTenant(t.Context(), p.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: p.Project.OrganizationID, ProjectID: p.Project.ID, WorkItemID: source.WorkItemID, ChangeID: change.ID, CreatedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
			return err
		}
		if _, err := w.UpdateChange(t.Context(), updated); err != nil {
			return err
		}
		if _, err := w.CreateLandingReceipt(t.Context(), receipt); err != nil {
			return err
		}
		return w.RecordVersionLanding(t.Context(), frozen)
	}); err != nil {
		t.Fatal(err)
	}
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-landing-source", 2, "succeeded"
	hubfixture.RequireStatus(t, p.Hub.Request(t, http.MethodPost, path, runner.Redemption.Credential, event), http.StatusOK)
	hubfixture.RequireStatus(t, p.Hub.Request(t, http.MethodPost, p.Base+"/leases/"+string(lease.ID)+"/release", runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}
