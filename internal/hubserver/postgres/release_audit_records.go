package postgres

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func releaseAuditRecord(row pgquery.ReleaseAuditCheckpoint) releaseaudit.Checkpoint {
	var c releaseaudit.Checkpoint
	c.ID = row.ID
	c.Identity.ProjectID = string(row.ProjectID)
	c.Identity.Repository = row.Repository
	c.Identity.Commit = row.CommitSha
	c.Identity.ReleaseSeries = row.ReleaseSeries
	c.Identity.RunID = row.RunID
	c.Identity.Attempt = row.RunAttempt
	c.Identity.JobID = row.JobID
	c.Identity.RunURL = row.RunUrl
	c.Sequence = int(row.Sequence)
	c.Status = row.Status
	c.BundleCommit = row.BundleCommit
	c.BundleDigest = row.BundleDigest
	c.Allowance.Invocations = int(row.AllowanceInvocations)
	c.Allowance.Usage.Input = row.AllowanceInput
	c.Allowance.Usage.Output = row.AllowanceOutput
	c.Allowance.Usage.Cached = row.AllowanceCached
	c.Invocations = int(row.Invocations)
	c.Usage.Input = row.InputTokens
	c.Usage.Output = row.OutputTokens
	c.Usage.Cached = row.CachedInputTokens
	c.Digest = row.Digest
	c.Bytes = int(row.Bytes)
	return c
}

func (s *session) ReleaseAudit(ctx context.Context, project tracker.ProjectID, id string) (releaseaudit.Checkpoint, error) {
	row, err := s.queries.ReleaseAudit(ctx, pgquery.ReleaseAuditParams{OrganizationID: s.organization, ProjectID: project, ID: id})
	if err != nil {
		return releaseaudit.Checkpoint{}, mapError(ctx, err)
	}
	c := releaseAuditRecord(row)
	c.Parts, err = s.releaseAuditParts(ctx, project, id)
	return c, err
}

func (s *session) releaseAuditParts(ctx context.Context, project tracker.ProjectID, id string) ([]releaseaudit.Part, error) {
	rows, err := s.queries.ReleaseAuditParts(ctx, pgquery.ReleaseAuditPartsParams{OrganizationID: s.organization, ProjectID: project, CheckpointID: id})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(rows) > 161 {
		return nil, eventstore.ErrInvalidRecord
	}
	parts := make([]releaseaudit.Part, 0, len(rows))
	for _, p := range rows {
		parts = append(parts, releaseaudit.Part{AttachmentID: p.AttachmentID, Index: int(p.PartIndex), Offset: int(p.ByteOffset), Bytes: int(p.Bytes), Digest: p.Digest})
	}
	return parts, nil
}

func (s *session) ReleaseAudits(ctx context.Context, project tracker.ProjectID, after string) (releaseaudit.CheckpointPage, error) {
	page := releaseaudit.CheckpointPage{Items: []releaseaudit.Checkpoint{}}
	if len(after) > 64 {
		return page, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ReleaseAudits(ctx, pgquery.ReleaseAuditsParams{OrganizationID: s.organization, ProjectID: project, AfterID: after})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(rows) > 20 {
		rows = rows[:20]
		page.Next = rows[19].ID
	}
	for _, row := range rows {
		c := releaseAuditRecord(row)
		c.Parts, err = s.releaseAuditParts(ctx, project, c.ID)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, c)
	}
	return page, nil
}

func (s *session) PutReleaseAudit(ctx context.Context, project tracker.ProjectID, c releaseaudit.Checkpoint, at time.Time) error {
	if !c.Valid() || c.Identity.ProjectID != string(project) || at.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	prior, err := s.ReleaseAudit(ctx, project, c.ID)
	if err == nil {
		if !reflect.DeepEqual(prior, c) {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	parts := slices.Clone(c.Parts)
	slices.SortFunc(parts, func(a, b releaseaudit.Part) int { return cmp.Compare(a.AttachmentID, b.AttachmentID) })
	for _, part := range parts {
		attachment, err := s.queries.LockReleaseAuditAttachment(ctx, pgquery.LockReleaseAuditAttachmentParams{OrganizationID: s.organization, ProjectID: project, ID: part.AttachmentID})
		if err != nil {
			return mapError(ctx, err)
		}
		meta := attachment
		if meta.DeletedAt != nil || attachment.ObjectDeletedAt != nil || meta.Size != int64(part.Bytes) || meta.Sha256 != part.Digest || meta.ContentType != "text/plain" {
			return eventstore.ErrInvalidRecord
		}
	}
	row := pgquery.CreateReleaseAuditParams{OrganizationID: s.organization, ProjectID: project, RecordedAt: at,
		ID:                   c.ID,
		Repository:           c.Identity.Repository,
		CommitSha:            c.Identity.Commit,
		ReleaseSeries:        c.Identity.ReleaseSeries,
		RunID:                c.Identity.RunID,
		RunAttempt:           c.Identity.Attempt,
		JobID:                c.Identity.JobID,
		RunUrl:               c.Identity.RunURL,
		Sequence:             int64(c.Sequence),
		Status:               c.Status,
		BundleCommit:         c.BundleCommit,
		BundleDigest:         c.BundleDigest,
		AllowanceInvocations: int64(c.Allowance.Invocations),
		AllowanceInput:       int64(c.Allowance.Usage.Input),
		AllowanceOutput:      int64(c.Allowance.Usage.Output),
		AllowanceCached:      int64(c.Allowance.Usage.Cached),
		Invocations:          int64(c.Invocations),
		InputTokens:          int64(c.Usage.Input),
		OutputTokens:         int64(c.Usage.Output),
		CachedInputTokens:    int64(c.Usage.Cached),
		Digest:               c.Digest,
		Bytes:                int64(c.Bytes),
	}
	count, err := s.queries.CreateReleaseAudit(ctx, row)
	if err != nil {
		return mapError(ctx, err)
	}
	if count == 0 {
		prior, err = s.ReleaseAudit(ctx, project, c.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(prior, c) {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	for _, part := range c.Parts {
		err = s.queries.CreateReleaseAuditPart(ctx, pgquery.CreateReleaseAuditPartParams{OrganizationID: s.organization, ProjectID: project, CheckpointID: c.ID, PartIndex: int64(part.Index), AttachmentID: part.AttachmentID, ByteOffset: int64(part.Offset), Bytes: int64(part.Bytes), Digest: part.Digest})
		if err != nil {
			return mapError(ctx, err)
		}
		if err = s.queries.RetainReleaseAuditAttachment(ctx, pgquery.RetainReleaseAuditAttachmentParams{OrganizationID: s.organization, ProjectID: project, ID: part.AttachmentID}); err != nil {
			return mapError(ctx, err)
		}
	}
	return mapError(ctx, s.queries.AdvanceReleaseAuditHead(ctx, pgquery.AdvanceReleaseAuditHeadParams{OrganizationID: s.organization, ProjectID: project, RunID: c.Identity.RunID, RunAttempt: c.Identity.Attempt, JobID: c.Identity.JobID, CheckpointID: c.ID, Sequence: int64(c.Sequence)}))
}
