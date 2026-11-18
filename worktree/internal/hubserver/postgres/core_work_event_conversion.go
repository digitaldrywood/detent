package postgres

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coreWorkEventFamily storageconversion.Family = "core-work-events"

func (s *session) commitCoreWorkEventPayload(ctx context.Context, id, issue int64, source string, target attachment.ArchiveReference) (bool, error) {
	if target.Validate(string(s.organization)) != nil || target.BindingID != "" || target.CustomerOwned || target.SchemaVersion != 1 {
		return false, storageconversion.ErrInvalid
	}
	var project tracker.ProjectID
	if err := s.db.QueryRow(ctx, `SELECT project_id FROM issues WHERE organization_id=$1 AND id=$2`, s.organization, issue).Scan(&project); err != nil {
		return false, mapError(ctx, err)
	}
	changed, err := s.queries.ConversionCommit(ctx, pgquery.ConversionCommitParams{OrganizationID: string(s.organization), ProjectID: string(project), Family: string(coreWorkEventFamily), RecordID: strconv.FormatInt(id, 10), RecordSequence: id, AggregateID: strconv.FormatInt(issue, 10), ExpectedVersion: 1, SourceKey: source, SourceContentBytes: int64(len(source)), TargetKey: target.Key, TargetDigest: target.SHA256, TargetBytes: target.CompressedSize, TargetContentBytes: target.UncompressedSize})
	return changed, mapError(ctx, err)
}

func (s *ConversionStorage) convertCoreWorkEvent(ctx context.Context, candidate storageconversion.Candidate, turn storageconversion.Turn) error {
	id := candidate.Position.Sequence
	issue, err := strconv.ParseInt(candidate.Scope.AggregateID, 10, 64)
	if err != nil || candidate.Scope.Organization != s.organization {
		return storageconversion.ErrInvalid
	}
	var raw string
	err = s.repository.transaction(ctx, s.organization, corestore.Background, true, func(session *session) error {
		return session.db.QueryRow(ctx, `SELECT payload_json FROM core_work_events WHERE organization_id=$1 AND source_id=$2 AND issue_id=$3 AND payload_object_key IS NULL`, s.organization, id, issue).Scan(&raw)
	})
	if err != nil {
		return err
	}
	if int64(len(raw)) != candidate.Source.UncompressedSize || !json.Valid([]byte(raw)) {
		return mutation.ErrConflict
	}
	if err := turn.Expand(len(raw) * 2); err != nil {
		return err
	}
	ref, err := s.content.PreparePayload(ctx, s.organization, []byte(raw))
	if err != nil {
		return err
	}
	return s.repository.transaction(ctx, s.organization, corestore.Background, false, func(session *session) error {
		_, err := session.commitCoreWorkEventPayload(ctx, id, issue, raw, ref)
		return err
	})
}
