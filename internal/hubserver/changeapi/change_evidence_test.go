package changeapi

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	reservation providercapacity.Requirement
}

func (w landingAuditEvidenceWriter) RunnerSession(context.Context, tracker.ProjectID, int64) (runnersession.Record, error) {
	return w.session, nil
}

func (w landingAuditEvidenceWriter) ProviderReservation(_ context.Context, lease tracker.LeaseID) (claimstore.ProviderReservation, error) {
	if lease != "lease_example" || w.reservation == (providercapacity.Requirement{}) {
		return claimstore.ProviderReservation{}, corestore.ErrNotFound
	}
	return claimstore.ProviderReservation{LeaseID: lease, Reservation: providercapacity.Reservation{Requirement: w.reservation}}, nil
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
	for _, test := range []struct {
		name        string
		reservation providercapacity.Requirement
		reported    string
		want        error
	}{
		{name: "matching session", reservation: match, reported: match.Model},
		{name: "missing reservation", reported: match.Model, want: refused},
		{name: "merge reservation", reservation: providercapacity.Requirement{Role: "merge", Backend: match.Backend, Model: match.Model}, reported: match.Model, want: refused},
		{name: "different backend", reservation: providercapacity.Requirement{Role: match.Role, Backend: "other", Model: match.Model}, reported: match.Model, want: refused},
		{name: "different model", reservation: providercapacity.Requirement{Role: match.Role, Backend: match.Backend, Model: "other"}, reported: match.Model, want: refused},
		{name: "misattributed verdict", reservation: match, reported: "other", want: refused},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := runnersession.Record{ID: 1, Role: match.Role, BackendID: match.Backend, Model: match.Model}
			writer := landingAuditEvidenceWriter{session: session, reservation: test.reservation}
			request := tracker.ReviewChange{Mutation: tracker.Mutation{LeaseID: "lease_example", FencingToken: 1}, SecurityAudit: &tracker.SecurityAuditReview{
				Landing: true, SessionID: session.ID, Identity: agentidentity.Identity{Role: match.Role, BackendID: match.Backend, RequestedModel: agentidentity.NewValue(test.reported, agentidentity.ProvenanceConfigured)},
			}}
			err := owner.lockValidatorReviewRecord(t.Context(), writer, scope, tracker.ChangeRequest{}, request, &preparedValidatorReview{session: session}, runnerstore.Identity{}, eventstore.Change{})
			if !errors.Is(err, test.want) {
				t.Fatalf("landing verdict error = %v, want %v", err, test.want)
			}
		})
	}
}
