package changeapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestValidatorArchiveChecksFenceBeforeLookup(t *testing.T) {
	t.Parallel()
	reader := &validatorArchiveReader{}
	server := NewHTTP(HTTPDeps{
		Repository: validatorArchiveRepository{reader: reader},
		AuthorizeAccessScope: func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error {
			return nil
		},
	})
	scope := hubscope.NativeScope{Organization: "org_example", Project: "prj_example"}
	mutation := tracker.Mutation{LeaseID: "lease_example", FencingToken: 1}
	_, err := server.readValidatorArchive(t.Context(), scope, "wi_example", changerequest.ValidatorArchiveReference{}, mutation)
	if !errors.Is(err, tracker.ErrStaleFencingToken) || reader.archiveLookups != 0 {
		t.Fatalf("archive lookups = %d, error = %v, want no lookup after fence rejection", reader.archiveLookups, err)
	}
}

func TestValidatorArchiveJSONBounds(t *testing.T) {
	t.Parallel()
	timing := "{\"receipt_id\":\"timing_example\",\"stage\":\"source_finalization\",\"command\":\"make check\",\"head_sha\":\"head\",\"tree_sha\":\"tree\",\"started_at\":\"2026-11-19T00:00:00Z\",\"finished_at\":\"2026-11-19T00:00:01Z\",\"exit_code\":0}"
	timings := strings.Repeat(timing+",", 1023) + timing
	for _, tc := range []struct {
		name  string
		input string
		valid bool
	}{
		{name: "long output", input: "{\"output\":\"" + strings.Repeat("{,[\\\"}", 200_000) + "\"}", valid: true},
		{name: "full pipeline timeline", input: "{\"timings\":[" + timings + "]}", valid: true},
		{name: "large collection", input: "{\"values\":[" + strings.Repeat("0,", 10_000) + "0]}"},
		{name: "many scalar object members", input: "{" + strings.Repeat("\"extra\":0,", 33_000) + "\"output\":\"\"}"},
		{name: "invalid UTF-8 in a string", input: "{\"output\":\"\xff\"}"},
		{name: "malformed syntax", input: "{\"review\":"},
		{name: "extra value", input: "{} {}"},
		{name: "wrong root type", input: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validValidatorArchiveJSON([]byte(tc.input)); got != tc.valid {
				t.Fatalf("validator archive JSON accepted = %t, want %t", got, tc.valid)
			}
		})
	}
}

type validatorArchiveRepository struct {
	tenantstore.Repository
	reader tenantstore.Reader
}

func (r validatorArchiveRepository) ReadTenant(_ context.Context, _ tracker.OrganizationID, _ corestore.Workload, read func(tenantstore.Reader) error) error {
	return read(r.reader)
}

type validatorArchiveReader struct {
	tenantstore.Reader
	archiveLookups int
}

func (r *validatorArchiveReader) RunnerForLease(context.Context, string) (runnerstore.Identity, error) {
	return runnerstore.Identity{}, tracker.ErrStaleFencingToken
}

func (r *validatorArchiveReader) AttachmentArchive(context.Context, attachmentstore.ArchiveQuery) (attachment.ArchiveReference, error) {
	r.archiveLookups++
	return attachment.ArchiveReference{}, corestore.ErrNotFound
}

type landingAuditEvidenceWriter struct {
	tenantstore.Writer
	runnersession.Reader
	eventstore.AuditDispositionReader
	session     runnersession.Record
	reservation providercapacity.Reservation
	predecessor *int64
}

func (w landingAuditEvidenceWriter) RunnerSession(context.Context, tracker.ProjectID, int64) (runnersession.Record, error) {
	return w.session, nil
}

func (w landingAuditEvidenceWriter) ProviderReservation(_ context.Context, lease tracker.LeaseID) (claimstore.ProviderReservation, error) {
	if lease != "lease_example" || w.reservation.Requirement == (providercapacity.Requirement{}) {
		return claimstore.ProviderReservation{}, corestore.ErrNotFound
	}
	return claimstore.ProviderReservation{LeaseID: lease, Reservation: w.reservation, AuditPredecessor: w.predecessor}, nil
}

func (landingAuditEvidenceWriter) AuditDispositions(context.Context, tracker.ProjectID, int64) ([]eventstore.AuditDisposition, error) {
	return nil, nil
}

func TestLandingAuditVerdictRequiresMatchingReservation(t *testing.T) {
	t.Parallel()
	scope := hubscope.NativeScope{Organization: "org_example", Project: "prj_example"}
	refused := errors.New("audit session does not match its reservation")
	owner := newValidator(ValidatorDeps{
		RunnerSessionFenceWithOwner: func(context.Context, tenantstore.Reader, hubscope.NativeScope, runnersession.Mutation, string, runnerstore.Identity) (leasestore.Attempt, corestore.WorkItem, policystore.Revision, error) {
			return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, nil
		},
		SecurityAuditRejection: func(string, string) error { return refused },
	})
	match := providercapacity.Requirement{Role: "security_audit", Backend: "audit", Model: "audit-model"}
	providerDefault := match
	providerDefault.Model = "provider_default"
	for _, test := range []struct {
		name          string
		reservation   providercapacity.Requirement
		requested     string
		reported      string
		observed      string
		finalized     string
		finalIdentity string
		verification  int64
		admission     int64
		sessionPhase  int64
		legacy        bool
		alias         bool
		want          error
	}{
		{name: "matching session", reservation: match, reported: match.Model},
		{name: "verification verdict cannot use primary admission", reservation: match, reported: match.Model, verification: 73, want: refused},
		{name: "verification binds predecessor even when models agree", reservation: match, reported: match.Model, verification: 73, admission: 73, sessionPhase: 73},
		{name: "primary session cannot publish against a later verification", reservation: match, reported: match.Model, verification: 73, admission: 73, want: refused},
		{name: "verification cannot replace its predecessor", reservation: match, reported: match.Model, verification: 75, admission: 73, sessionPhase: 73, want: refused},
		{name: "verifier cannot publish as a primary session", reservation: match, reported: match.Model, admission: 73, sessionPhase: 73, want: refused},
		{name: "recorded catalog alias", reservation: match, reported: match.Model, observed: "audit-alias", finalized: "audit-alias", finalIdentity: "audit-alias", alias: true},
		{name: "missing reservation", reported: match.Model, want: refused},
		{name: "merge reservation", reservation: providercapacity.Requirement{Role: "merge", Backend: match.Backend, Model: match.Model}, reported: match.Model, want: refused},
		{name: "different backend", reservation: providercapacity.Requirement{Role: match.Role, Backend: "other", Model: match.Model}, reported: match.Model, want: refused},
		{name: "different model", reservation: providercapacity.Requirement{Role: match.Role, Backend: match.Backend, Model: "other"}, reported: match.Model, want: refused},
		{name: "misattributed verdict", reservation: match, reported: "other", want: refused},
		{name: "runtime model differs", reservation: match, reported: match.Model, observed: "other", want: refused},
		{name: "immutable finish model differs after mutable identity was changed", reservation: match, reported: match.Model, observed: match.Model, finalized: "other", finalIdentity: match.Model, want: refused},
		{name: "immutable finish identity differs after mutable identity was changed", reservation: match, reported: match.Model, observed: match.Model, finalized: match.Model, finalIdentity: "other", want: refused},
		{name: "legacy finish differs after mutable identity was changed", reservation: match, reported: match.Model, observed: match.Model, finalized: "other", legacy: true, want: refused},
		{name: "provider default accepts observed model", reservation: providerDefault, requested: "provider_default", observed: "chosen-model", finalized: "chosen-model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := match.Model
			if test.requested != "" {
				model = test.requested
			}
			session := runnersession.Record{ID: 1, Role: match.Role, BackendID: match.Backend, Model: model}
			report := providercapacity.Report{}
			if test.alias {
				report.Models = []string{"audit-model", "audit-alias"}
				report.ModelDetails = []providercapacity.ModelDetail{{ID: "audit-alias", CanonicalModel: "audit-model"}}
			}
			writer := landingAuditEvidenceWriter{session: session, predecessor: new(test.admission), reservation: providercapacity.Reservation{Requirement: test.reservation, Report: report}}
			request := tracker.ReviewChange{Mutation: tracker.Mutation{LeaseID: "lease_example", FencingToken: 1}, SecurityAudit: &tracker.SecurityAuditReview{
				Landing: true, SessionID: session.ID, Identity: agentidentity.Identity{Role: match.Role, BackendID: match.Backend, RequestedModel: agentidentity.NewValue(test.reported, agentidentity.ProvenanceConfigured), ResolvedModel: agentidentity.NewValue(test.observed, agentidentity.ProvenanceRuntime)},
			}}
			if test.verification != 0 {
				request.SecurityAudit.Run.Verification = &securityaudit.Verification{InitialSessionID: test.verification}
			}
			finish := store.SessionFinish{Model: test.finalized, RuntimeIdentity: agentidentity.RuntimeUpdate(test.finalIdentity, "", "", "", time.Time{})}
			snapshot := runnersession.Snapshot{Identity: agentidentity.RuntimeUpdate(model, "", "", "", time.Time{}), Finish: &finish}
			if !test.legacy {
				snapshot.Finish = &store.SessionFinish{Model: model}
				snapshot.Finalization = &runnersession.Finalization{Input: sessionmodel.Finalization{Finish: finish}}
			}
			prepared := &preparedValidatorReview{session: session, auditPredecessor: new(test.sessionPhase), auditObservedModels: auditFinishModels(snapshot)}
			err := owner.lockValidatorReviewRecord(t.Context(), writer, scope, tracker.ChangeRequest{}, request, prepared, runnerstore.Identity{}, eventstore.Change{})
			if !errors.Is(err, test.want) {
				t.Fatalf("landing verdict error = %v, want %v", err, test.want)
			}
		})
	}
}
