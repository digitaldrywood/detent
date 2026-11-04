package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/validation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAuditDispositionIdentityAndReplay(t *testing.T) {
	f := newRunnerPGFixture(t, func(p *policy.Descriptor) { p.Gates.SecurityAudit = true })
	for index, lease := range f.leases {
		native, ctx := f.client(t, index)
		now := time.Now().UTC().Truncate(time.Microsecond)
		sessionID, err := api.NewRunnerSessions(native).StartSession(ctx, store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: now, AgentRole: "security_audit", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		key := securityaudit.Key{ProjectID: "prj_runner", Repository: "example-org/example-project", PRNumber: index + 1, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat(strconv.Itoa(index+1), 40)}
		changeID, versionID := newNativeID("change"), newNativeID("version")
		change := tracker.ChangeRequest{ID: changeID, OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{lease.WorkItemID}, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
		changeRaw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		changeRow, err := content.PrepareChange(ctx, "org_startup", changeRaw)
		if err != nil {
			t.Fatal(err)
		}
		version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, CreatedAt: now, ChangeVersionInput: tracker.ChangeVersionInput{Repository: key.Repository, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, AttemptID: f.data[index].AttemptID, PolicyID: f.policy.PolicyID}}
		version.Policy, err = content.ProjectPolicy(ctx, f.policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/change-review-policy", nil, &version.ReviewPolicy); err != nil {
			t.Fatal(err)
		}
		version.Checks = []tracker.ChangeCheckExpectation{{CheckRunID: "pending-example"}}
		versionRaw, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		versionRow, err := content.PrepareVersion(ctx, "org_startup", versionRaw)
		if err != nil {
			t.Fatal(err)
		}
		run := securityaudit.Run{Verdict: securityaudit.VerdictFail, ID: sessionID, IssueID: string(lease.WorkItemID), ProjectID: "prj_runner", Repository: key.Repository, PRNumber: key.PRNumber, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, InvocationID: "audit-example", WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, OutputBytes: 2, OutputDigest: securityaudit.OutputDigest("{}"), Summary: "Audited", ExitStatus: securityaudit.ExitStatusSuccess, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), Findings: []securityaudit.Finding{{ID: "finding_example", Severity: "p1", Body: "Example finding", Certainty: securityaudit.CertaintyConfirmed}}}
		if index == 1 {
			run.Findings = append(run.Findings, securityaudit.Finding{ID: "finding_unresolved", Severity: "p1", Body: "Another finding", Certainty: securityaudit.CertaintyConfirmed})
		}
		review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: versionID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: sessionID, VersionID: versionID, Run: run}}
		reviewRaw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		reviewRow, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, reviewRaw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(ctx, changeRow); err != nil {
				return err
			}
			if _, err := w.CreateVersion(ctx, versionRow); err != nil {
				return err
			}
			if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, ChangeID: changeID}); err != nil {
				return err
			}
			changeRow.CurrentVersionID = &versionID
			if _, err := w.UpdateChange(ctx, changeRow); err != nil {
				return err
			}
			_, err := w.AppendEvidence(ctx, reviewRow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertGate := func(want bool) {
			t.Helper()
			detail, err := native.RuntimeChange(ctx, lease.WorkItemID, changeID)
			if err != nil {
				t.Fatal(err)
			}
			var history operatortool.ChangeResult
			path := native.Scope() + "/work-items/" + string(lease.WorkItemID) + "/changes/" + changeID + "?view=bounded&section=reviews&version_id=" + versionID
			if err := native.Transport().Request(ctx, http.MethodGet, path, nil, &history); err != nil {
				t.Fatal(err)
			}
			if history.Detail == nil || len(history.Detail.Reviews) != 1 || !reflect.DeepEqual(history.Detail.Reviews[0], review) {
				t.Fatalf("immutable review changed: %+v", history.Detail)
			}
			if detail.Summary.Status == "reviewed" {
				t.Fatalf("canonical summary reviewed=%t, want %t: %+v", detail.Summary.Status == "reviewed", want, detail.Summary)
			}
			connector, err := api.NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			state, err := connector.ReadChangeReview(ctx, string(lease.WorkItemID), changeID, versionID, version.HeadSHA)
			if err != nil || state.Reviewed || state.Current && state.ChangesRequested == want {
				t.Errorf("canonical gate: %+v, want audit cleared=%t: %v", state, want, err)
			}

			if detail.Change.CurrentVersion == versionID {
				host := &auditDispositionVersionAuthority{}
				change := &runner.NativeChange{ChangeID: changeID, VersionID: versionID}
				prepared, err := validation.PrepareVersion(ctx, native, host, validation.VersionState{Change: change, Diff: &tracker.AttemptDiffRequest{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}, Lease: lease}, "security_audit")
				if err != nil || prepared.Version != nil || change.Reviewed || (change.SecurityAudit == nil) != want || host.actions != map[bool]int{true: 0, false: 1}[want] {
					t.Fatalf("audit consumer: prepared=%+v change=%+v actions=%d cleared=%t error=%v", prepared, change, host.actions, want, err)
				}
			}
		}
		assertGate(false)
		reviewRevision := func() int64 {
			t.Helper()
			var revision int64
			if err := f.service.config.TenantRepository.ReadTenant(ctx, "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.Change(ctx, changeID)
				revision = row.ReviewEvidenceRevision
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return revision
		}
		beforeDisposition := reviewRevision()
		command := securityaudit.DispositionCommand{RequestID: "audit-replay", Key: key, FindingID: "finding_example", Evidence: "The exact source requires authenticated access.", Confirm: true}
		disposition, err := native.RecordAuditDisposition(ctx, command)
		if err != nil || disposition.ID < 1 || disposition.AuditRunID != sessionID {
			t.Fatalf("Hub disposition: %+v %v", disposition, err)
		}
		if got := reviewRevision(); got != beforeDisposition+1 {
			t.Fatalf("audit disposition did not invalidate review snapshot: got %d, want %d", got, beforeDisposition+1)
		}
		assertGate(index == 0)
		restarted, restartedCtx := f.client(t, index)
		evidence, err := restarted.AuditEvidence(restartedCtx, key)
		if err != nil || evidence.Run.ID != sessionID || len(evidence.Dispositions) != 1 || evidence.Dispositions[0].Evidence != disposition.Evidence {
			t.Fatalf("restart lost audit disposition: %+v %v", evidence, err)
		}
		if disposition.ServiceIdentity != securityaudit.ServiceIdentity("prj_runner") {
			t.Errorf("disposition has actor identity instead of trusted project owner: %q", disposition.ServiceIdentity)
		}
		scope := nativeScope{organization: "org_startup", project: "prj_runner"}
		if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			credential, err := r.AccessCredential(ctx, "credential_"+strconv.Itoa(index+1))
			scope.credential = apiCredential{ID: credential.ID, Hash: credential.Hash, Scope: apiScope(credential.Scope)}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		values, receipts, err := f.service.prepareValidatorAuditDispositions(ctx, scope, changeID, run)
		if err != nil || len(values) != 1 {
			t.Fatalf("trusted owner preparation: %+v %v", values, err)
		}
		evaluation := securityaudit.Evaluate(run, values, key, securityaudit.ServiceIdentity("prj_runner"), []string{"p1"})
		if evaluation.Allowed != (index == 0) {
			t.Fatalf("exact-source disposition did not clear finding: %+v", evaluation)
		}
		if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if receipts[0].ActorID != scope.credential.ID {
				t.Fatalf("actor attribution lost: %+v", receipts[0])
			}
			return lockValidatorAuditDispositions(ctx, r, scope, run.ID, receipts)
		}); err != nil {
			t.Fatal(err)
		}
		descriptor := hubTestPolicy()
		descriptor.Gates.SecurityAudit = true
		validationVersion := version
		validationVersion.Policy = descriptor.WithID()
		validationChange := change
		validationChange.CurrentVersion = versionID
		validationReview := tracker.ReviewChange{Decision: "approved", Mutation: tracker.Mutation{FencingToken: lease.FencingToken}, SecurityAudit: &tracker.SecurityAuditReview{VersionID: versionID, SessionID: sessionID, DiffDigest: strings.Repeat("d", 64), Verdict: gate.ValidatorVerdictPass, Run: run}}
		validationEvidence := validatorReviewEvidence{issue: tracker.NativeIssue{}, sourceSession: sessionID + 100, sourceMachine: "example_source", current: f.data[index], auditDispositions: values, auditDispositionRows: receipts}
		if _, err := validateValidatorReviewEvidence(scope, validationChange, validationVersion, validationReview, validationEvidence); (err == nil) != (index == 0) {
			t.Fatalf("authoritative exact-source clearing rejected: %v", err)
		}
		validationEvidence.auditDispositions = nil
		if _, err := validateValidatorReviewEvidence(scope, validationChange, validationVersion, validationReview, validationEvidence); err == nil {
			t.Fatal("authoritative validator accepted unresolved finding")
		}
		wrongSource := run
		wrongSource.HeadSHA = strings.Repeat("f", 40)
		if _, _, err := f.service.prepareValidatorAuditDispositions(ctx, scope, changeID, wrongSource); err == nil {
			t.Fatal("wrong source inherited a disposition")
		}
		forged := values[0]
		forged.ServiceIdentity = scope.credential.ID
		if securityaudit.Evaluate(run, []securityaudit.Disposition{forged}, key, securityaudit.ServiceIdentity("prj_runner"), []string{"p1"}).Allowed {
			t.Fatal("actor identity forged audit owner")
		}
		if index == 0 {
			next := version
			next.ID, next.Number, next.HeadSHA = newNativeID("version"), 2, strings.Repeat("f", 40)
			raw, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			row, err := content.PrepareVersion(ctx, "org_startup", raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateVersion(ctx, row); err != nil {
					return err
				}
				current, err := w.Change(ctx, changeID)
				if err != nil {
					return err
				}
				current.CurrentVersionID = &next.ID
				_, err = w.UpdateChange(ctx, current)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertGate(false)
		}
		review.SecurityAudit.Run.Findings = nil
		review.CreatedAt = now.Add(time.Second)
		reviewRaw, err = json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		newer, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: review.CreatedAt}, reviewRaw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(ctx, newer); return err }); err != nil {
			t.Fatal(err)
		}
		replay, err := restarted.RecordAuditDisposition(restartedCtx, command)
		if err != nil || replay != disposition {
			t.Errorf("lost acknowledgement replay changed archived receipt: %+v %v", replay, err)
		}
		if got := reviewRevision(); got != beforeDisposition+1 {
			t.Fatalf("audit receipt replay changed review snapshot: got %d, want %d", got, beforeDisposition+1)
		}
		other := performHubAPIRequest(t, f.service, http.MethodPost, native.Scope()+"/security-audits/dispositions", f.tokens[1-index], command)
		if other.Code == http.StatusOK {
			t.Fatal("another actor inherited committed receipt")
		}
		changed := command
		changed.Evidence = "Changed payload"
		response := performHubAPIRequest(t, f.service, http.MethodPost, native.Scope()+"/security-audits/dispositions", f.tokens[index], changed)
		requireNativeStatus(t, response, http.StatusConflict)
		key.HeadSHA = strings.Repeat("f", 40)
		if _, err := native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: key, FindingID: "finding_example", Evidence: "Wrong source", Confirm: true}); err == nil {
			t.Fatal("unrecorded audit source accepted a disposition")
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			return w.(accessstore.Writer).DeleteAccessTokenGrant(ctx, scope.credential.ID, "prj_runner")
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.RecordAuditDisposition(restartedCtx, command); err == nil {
			t.Fatal("revoked current grant replayed a receipt")
		}

	}
}

type auditDispositionVersionAuthority struct{ actions int }

func (*auditDispositionVersionAuthority) Retain(context.Context, tracker.AttemptDiffRequest) error {
	return nil
}
func (*auditDispositionVersionAuthority) Retained() *tracker.ChangeSourceCapture { return nil }
func (*auditDispositionVersionAuthority) SetDiscussion([]tracker.NativeComment)  {}
func (h *auditDispositionVersionAuthority) ApplyAuditHumanAction(*tracker.SecurityAuditReview) {
	h.actions++
}
