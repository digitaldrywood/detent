package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ArtifactService(ctx context.Context, project tracker.ProjectID, id string) (artifact.Binding, error) {
	row, err := s.queries.ArtifactServiceGet(ctx, pgquery.ArtifactServiceGetParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	return artifactServiceBinding(row), mapError(ctx, err)
}

func (s *session) ArtifactServices(ctx context.Context, project tracker.ProjectID) ([]artifact.Binding, error) {
	rows, err := s.queries.ArtifactServiceList(ctx, pgquery.ArtifactServiceListParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]artifact.Binding, 0, len(rows))
	for _, row := range rows {
		result = append(result, artifactServiceBinding(row))
	}
	return result, nil
}

func (s *session) BindArtifactService(ctx context.Context, project tracker.ProjectID, binding artifact.Binding) error {
	if !artifact.ValidID(binding.ServiceID, "service") || !artifact.ValidOrigin(binding.Origin) || !slices.Contains([]string{"customer", "hosted"}, binding.Mode) || binding.Mode == "hosted" && !binding.HostedOptIn || binding.PublisherTokenID == "" {
		return corestore.ErrInvalidCursor
	}
	count, err := s.queries.ArtifactServiceBind(ctx, pgquery.ArtifactServiceBindParams{OrganizationID: s.organization, ProjectID: project, ID: binding.ServiceID, Origin: binding.Origin, BindingMode: binding.Mode, HostedOptIn: binding.HostedOptIn, PublisherTokenID: binding.PublisherTokenID})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func artifactServiceBinding(row pgquery.ArtifactService) artifact.Binding {
	return artifact.Binding{ServiceID: row.ID, Origin: row.Origin, Mode: row.BindingMode, HostedOptIn: row.HostedOptIn, PublisherTokenID: row.PublisherTokenID}
}

func (s *session) RecordHostedArtifactUsage(ctx context.Context, service, publisher string, usage artifact.Usage, now time.Time, windowSeconds int64) error {
	if now.IsZero() || windowSeconds <= 0 || usage.ObservedAt.After(now.Add(time.Minute)) {
		return corestore.ErrInvalidCursor
	}
	for _, amount := range []int64{usage.RetainedBytes, usage.ReservedBytes, usage.RelayBytes, usage.StorageRequests} {
		if amount < 0 || amount > 1<<50 {
			return corestore.ErrInvalidCursor
		}
	}
	bound, err := s.queries.HostedArtifactPublisherBound(ctx, pgquery.HostedArtifactPublisherBoundParams{OrganizationID: s.organization, ServiceID: service, PublisherID: publisher, Now: recordTime(now)})
	if err != nil {
		return mapError(ctx, err)
	}
	if !bound {
		return corestore.ErrNotFound
	}
	if err := s.queries.HostedArtifactUsageEnsure(ctx, pgquery.HostedArtifactUsageEnsureParams{OrganizationID: s.organization, ServiceID: service}); err != nil {
		return mapError(ctx, err)
	}
	current, err := s.queries.HostedArtifactUsageLock(ctx, s.organization)
	if err != nil {
		return mapError(ctx, err)
	}
	if current.ServiceID != service {
		return artifact.ErrQuota
	}
	if usage.ObservedAt.IsZero() || !usage.ObservedAt.After(current.ObservedAt) {
		return nil
	}
	if err := s.queries.HostedArtifactUsageUpdate(ctx, pgquery.HostedArtifactUsageUpdateParams{OrganizationID: s.organization, ObservedAt: recordTime(usage.ObservedAt), RetainedBytes: usage.RetainedBytes, ReservedBytes: usage.ReservedBytes}); err != nil {
		return mapError(ctx, err)
	}
	window := time.Unix(now.Unix()/windowSeconds*windowSeconds, 0).UTC()
	if usage.WindowStart.Unix() != window.Unix() {
		return nil
	}
	for _, sample := range []struct {
		metric string
		amount int64
	}{{"relay_bytes", usage.RelayBytes}, {"storage_requests", usage.StorageRequests}} {
		if err := s.queries.HostedArtifactUsageMetric(ctx, pgquery.HostedArtifactUsageMetricParams{OrganizationID: s.organization, WindowStart: window, Metric: sample.metric, Amount: sample.amount}); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
