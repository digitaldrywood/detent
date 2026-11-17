package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledSourceKey struct {
	project  tracker.ProjectID
	item     tracker.NativeWorkItemID
	record   string
	revision int64
	observed time.Time
	body     *string
}

func (s *session) classifyScheduledSource(ctx context.Context, row eventstore.CollaborationVersion, body *string, onlyPending bool) (int64, error) {
	if !row.SourceKnown {
		return 0, nil
	}
	var count int64
	var err error
	if row.RecordID == string(row.WorkItemID) {
		count, err = s.queries.RecordClassifyScheduledIssue(ctx, pgquery.RecordClassifyScheduledIssueParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, Revision: row.Revision, SourceScheduled: row.SourceScheduled, SourceObservedAt: row.SourceObservedAt, ExpectedBodyObjectKey: body, OnlyPending: onlyPending})
	} else {
		count, err = s.queries.RecordClassifyScheduledComment(ctx, pgquery.RecordClassifyScheduledCommentParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, RecordID: row.RecordID, Revision: row.Revision, SourceScheduled: row.SourceScheduled, SourceObservedAt: row.SourceObservedAt, ExpectedBodyObjectKey: body, OnlyPending: onlyPending})
	}
	return count, mapError(ctx, err)
}

func (s *session) readScheduledSourceVersions(ctx context.Context, keys []scheduledSourceKey) ([]eventstore.CollaborationVersion, error) {
	versions := make([]eventstore.CollaborationVersionKey, len(keys))
	for i, key := range keys {
		versions[i] = eventstore.CollaborationVersionKey{RecordID: key.record, Revision: key.revision}
	}
	rows, err := s.CollaborationVersions(ctx, versions)
	if err != nil {
		return nil, err
	}
	byKey := map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion{}
	for _, row := range rows {
		byKey[eventstore.CollaborationVersionKey{RecordID: row.RecordID, Revision: row.Revision}] = row
	}
	result := make([]eventstore.CollaborationVersion, len(keys))
	for i, key := range keys {
		row, ok := byKey[versions[i]]
		if !ok || row.OrganizationID != s.organization || row.ProjectID != key.project || row.WorkItemID != key.item {
			return nil, artifact.ErrIntegrity
		}
		result[i] = row
	}
	return result, nil
}

type ScheduledSourceCursor struct {
	ProjectID tracker.ProjectID
	RecordID  string
}

func (r *Repository) BackfillScheduledSources(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int) (int, error) {
	count, _, err := r.BackfillScheduledSourcesAfter(ctx, organization, content, limit, ScheduledSourceCursor{})
	return count, err
}

func (r *Repository) BackfillScheduledSourcesAfter(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int, after ScheduledSourceCursor) (int, ScheduledSourceCursor, error) {
	if organization == "" || content == nil || limit < 1 || limit > corestore.MaxPageSize {
		return 0, after, corestore.ErrInvalidPage
	}
	keys := []scheduledSourceKey{}
	var versions []eventstore.CollaborationVersion
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		rows, err := s.queries.RecordUnclassifiedScheduledSources(ctx, pgquery.RecordUnclassifiedScheduledSourcesParams{OrganizationID: organization, PageLimit: int32(limit), AfterProjectID: string(after.ProjectID), AfterRecordID: after.RecordID})
		if err != nil {
			return mapError(ctx, err)
		}
		requested := make([]eventstore.CollaborationVersionKey, len(rows))
		for i, row := range rows {
			var body *string
			if !row.BodyMissing {
				body = new(row.BodyObjectKey)
			}
			keys = append(keys, scheduledSourceKey{project: row.ProjectID, item: tracker.NativeWorkItemID(row.WorkItemID), record: row.RecordID, revision: row.Revision, body: body})
			requested[i] = eventstore.CollaborationVersionKey{RecordID: row.RecordID, Revision: row.Revision}
		}
		versions, err = s.CollaborationVersions(ctx, requested)
		return err
	})
	if err != nil {
		return 0, after, fmt.Errorf("read scheduled source backfill: %w", err)
	}
	if len(keys) == 0 {
		return 0, ScheduledSourceCursor{}, nil
	}
	byKey := map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion{}
	for _, row := range versions {
		byKey[eventstore.CollaborationVersionKey{RecordID: row.RecordID, Revision: row.Revision}] = row
	}
	count := 0
	next := after
	budget := scheduledSourceContentLimit
	archiveBudget := scheduledSourceArchiveLimit
	var failures []error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return count, next, errors.Join(append(failures, err)...)
		}
		row, ok := byKey[eventstore.CollaborationVersionKey{RecordID: key.record, Revision: key.revision}]
		var sizeErr error
		if ok {
			fits, err := scheduledSourceFitsPage(row.PayloadBytes, row.PayloadContentBytes, budget, archiveBudget)
			sizeErr = err
			if !fits && err == nil {
				break
			}
		}
		next = ScheduledSourceCursor{ProjectID: key.project, RecordID: key.record}
		apply := func() error {
			if !ok || row.OrganizationID != organization || row.ProjectID != key.project || row.WorkItemID != key.item {
				return artifact.ErrIntegrity
			}
			if sizeErr != nil {
				return sizeErr
			}
			budget -= row.PayloadContentBytes
			archiveBudget -= row.PayloadBytes + row.PayloadContentBytes
			raw, err := content.CollaborationVersion(ctx, row)
			if err != nil {
				return err
			}
			projected, err := eventstore.ProjectScheduledSource(row, raw)
			if err != nil {
				return err
			}
			if !projected.SourceKnown {
				return artifact.ErrIntegrity
			}
			var changed int64
			err = r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
				var err error
				changed, err = s.classifyScheduledSource(ctx, projected, key.body, true)
				return err
			})
			if err == nil {
				count += int(changed)
			}
			return err
		}
		if err := apply(); err != nil {
			failures = append(failures, fmt.Errorf("classify scheduled source %s revision %d: %w", key.record, key.revision, err))
		}
	}
	return count, next, errors.Join(failures...)
}

const scheduledSourceContentLimit int64 = 32 << 20
const scheduledSourceArchiveLimit int64 = 64 << 20

func scheduledSourceFitsPage(compressed, content, remainingContent, remainingArchive int64) (bool, error) {
	if compressed <= 0 || compressed > scheduledSourceContentLimit || content <= 0 || content > scheduledSourceContentLimit {
		return false, artifact.ErrIntegrity
	}
	return content <= remainingContent && compressed <= remainingArchive-content, nil
}
