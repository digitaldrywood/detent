package changeapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
