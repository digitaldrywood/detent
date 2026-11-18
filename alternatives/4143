package workitemread

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (rd Reader) LinkedSource(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, item tracker.NativeWorkItemID) (tracker.LinkedIssueSource, bool, error) {
	var record eventstore.LinkedSource
	err := rd.deps.Tenants.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.LinkedSource(ctx, project, item)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.LinkedIssueSource{}, false, nil
	}
	if err != nil {
		return tracker.LinkedIssueSource{}, false, hubapi.ProjectRecordError(err)
	}
	result, err := rd.HydrateLinkedSource(ctx, organization, record)
	if err != nil {
		return tracker.LinkedIssueSource{}, false, err
	}
	return result, true, nil
}

func (rd Reader) HydrateLinkedSource(ctx context.Context, organization tracker.OrganizationID, record eventstore.LinkedSource) (tracker.LinkedIssueSource, error) {
	result := &tracker.LinkedIssueSource{URL: record.URL, Status: "pending"}
	if record.SnapshotObjectKey == "" {
		return *result, nil
	}
	snapshot, err := eventstore.NewContent(rd.deps.Details).LinkedIssueSnapshot(ctx, organization, eventstore.Reference(record.SnapshotObjectKey, record.SnapshotSHA256, record.SnapshotBytes, record.SnapshotContentBytes))
	if err != nil {
		return tracker.LinkedIssueSource{}, err
	}
	if snapshot.URL != record.URL {
		return tracker.LinkedIssueSource{}, artifact.ErrIntegrity
	}
	result.Status = "complete"
	result.Snapshot = &snapshot
	return *result, nil
}
