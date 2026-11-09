package hubserver

import (
	"context"
	"maps"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthAttemptSourceReader struct {
	attemptMetadataSnapshotReader
	attempts map[string]leasestore.Attempt
}

func readHealthAttemptSources(ctx context.Context, r tenantstore.Reader, organization tracker.OrganizationID, project tracker.ProjectID, ids []string) (tenantstore.Reader, error) {
	result := healthAttemptSourceReader{attemptMetadataSnapshotReader: attemptMetadataSnapshotReader{Reader: r, organization: organization}, attempts: map[string]leasestore.Attempt{}}
	if len(ids) == 0 {
		return result, nil
	}
	owner, ok := r.(healthstore.AttemptSourceReader)
	if !ok {
		return nil, workItemStorageUnavailable()
	}
	for len(ids) > 0 {
		count := min(len(ids), corestore.MaxPageSize)
		sources, err := owner.HealthAttemptSources(ctx, project, ids[:count])
		if err != nil {
			return nil, err
		}
		maps.Copy(result.attempts, sources.Attempts)
		if result.metadata.Leases == nil {
			result.metadata = sources.Metadata
		} else {
			maps.Copy(result.metadata.Leases, sources.Metadata.Leases)
			maps.Copy(result.metadata.Events, sources.Metadata.Events)
			maps.Copy(result.metadata.Archives, sources.Metadata.Archives)
			maps.Copy(result.metadata.Attachments, sources.Metadata.Attachments)
		}
		ids = ids[count:]
	}
	return result, nil
}

func (r healthAttemptSourceReader) Attempt(_ context.Context, id string) (leasestore.Attempt, error) {
	value, ok := r.attempts[id]
	if !ok {
		return value, corestore.ErrNotFound
	}
	return value, nil
}
