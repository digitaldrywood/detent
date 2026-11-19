package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/claimapi"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const recoveryCommentProgress = "recovery.comments_examined"
const recoveryCommentPagesPerRequest = 2

func (s *Service) recordedWorkpadQuestion(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, attempt leasestore.Attempt, advance bool) (bool, error) {
	var after, lastEdit int64
	var progress eventstore.Event
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		events, ok := r.(eventstore.AttemptEventPageReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		start, err := events.AttemptEvents(ctx, scope.Project, row.NativeID, attempt.ID, 0, 1)
		if err != nil {
			return err
		}
		if len(start) == 0 {
			return nil
		}
		after = start[0].Sequence
		reader, ok := r.(eventstore.WorkflowTransitionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		id, edit, err := reader.RecoveryCommentHistory(ctx, scope.Project, row.NativeID)
		lastEdit = edit
		if err != nil || id == "" {
			return err
		}
		progress, err = r.RunEvent(ctx, scope.Project, row.NativeID, id)
		return err
	})
	if err != nil {
		return false, err
	}
	if after == 0 {
		return true, nil
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	if progress.Sequence > after {
		event, err := content.Event(ctx, progress)
		if err != nil {
			return false, err
		}
		if event.Type != recoveryCommentProgress || event.Actor.Kind != "system" || event.Actor.PrincipalID != "recorded_recovery" || event.Data.CommentCursor >= progress.Sequence {
			return false, artifact.ErrIntegrity
		}
		if event.Data.BlockerAttemptID == attempt.ID {
			after = max(after, event.Data.CommentCursor)
		}
	}
	for range recoveryCommentPagesPerRequest {
		page, err := s.readRecoveryCommentPage(ctx, scope, row, after, lastEdit)
		if err != nil {
			return false, err
		}
		question, err := s.recoveryPageQuestion(ctx, scope, row.NativeID, page)
		if err != nil || question {
			return question, err
		}
		after = page.through
		if after >= row.EventSequence {
			return false, nil
		}
	}
	if advance {
		if err := s.recordRecoveryCommentProgress(ctx, scope, row, attempt.ID, after); err != nil {
			return false, err
		}
	}
	return false, corestore.ErrRevisionConflict
}

type recoveryCommentVersion struct {
	row     corestore.Comment
	version eventstore.CollaborationVersion
}

type recoveryCommentPage struct {
	through  int64
	comments map[string]recoveryCommentVersion
	edits    []eventstore.Event
}

func (p *recoveryCommentPage) add(ctx context.Context, r tenantstore.Reader, row corestore.Comment) error {
	if row.Deleted || row.ActorKind != "runner" && row.ActorKind != "human" && hubapi.StringValue(row.EditedByKind) != "human" {
		return nil
	}
	if _, ok := p.comments[row.ID]; ok {
		return nil
	}
	version, err := r.CollaborationVersion(ctx, row.ID, row.Revision)
	if err == nil {
		p.comments[row.ID] = recoveryCommentVersion{row, version}
	}
	return err
}

func (s *Service) recoveryPageQuestion(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, page recoveryCommentPage) (bool, error) {
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	editedIDs := make(map[string]struct{})
	for _, edited := range page.edits {
		event, err := content.Event(ctx, edited)
		if err != nil {
			return false, err
		}
		if event.Data.CommentID == "" {
			return false, artifact.ErrIntegrity
		}
		if _, ok := page.comments[event.Data.CommentID]; !ok {
			editedIDs[event.Data.CommentID] = struct{}{}
		}
	}
	if len(editedIDs) > 0 {
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			for id := range editedIDs {
				comment, err := r.Comment(ctx, scope.Project, item, id)
				if err != nil {
					return err
				}
				if err := page.add(ctx, r, comment); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	for _, record := range page.comments {
		comment, err := s.hydrateCommentRecord(ctx, record.row, record.version)
		if err != nil {
			return false, err
		}
		if claimapi.RecordedWorkpadQuestion(comment.Body) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) readRecoveryCommentPage(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, after, lastEdit int64) (recoveryCommentPage, error) {
	page := recoveryCommentPage{through: row.EventSequence, comments: make(map[string]recoveryCommentVersion)}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		comments, err := r.Comments(ctx, scope.Project, row.NativeID, after, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(comments) == corestore.MaxPageSize {
			page.through = min(page.through, comments[len(comments)-1].Sequence)
		}
		if lastEdit > after {
			events, err := r.Events(ctx, scope.Project, row.NativeID, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			if len(events) == corestore.MaxPageSize && events[len(events)-1].Sequence < lastEdit {
				page.through = min(page.through, events[len(events)-1].Sequence)
			}
			for _, event := range events {
				if event.Type == "comment.edited" && event.Sequence <= page.through {
					page.edits = append(page.edits, event)
				}
			}
		}
		for _, comment := range comments {
			if comment.Sequence > page.through {
				break
			}
			if err := page.add(ctx, r, comment); err != nil {
				return err
			}
		}
		return nil
	})
	return page, err
}

func (s *Service) recordRecoveryCommentProgress(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, attempt string, through int64) error {
	event := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: row.NativeID, Type: recoveryCommentProgress, SchemaVersion: 1,
		Actor: tracker.Actor{Kind: "system", PrincipalID: "recorded_recovery"}, RecordedAt: s.config.Clock().UTC(), Data: tracker.CollaborationData{BlockerAttemptID: attempt, CommentCursor: through}}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	prepared, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return err
	}
	prepared.HistoryHidden = true
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		locked, err := w.LockWorkItem(ctx, scope.Project, row.NativeID)
		if err != nil {
			return err
		}
		if locked.EventSequence != row.EventSequence || locked.Revision != row.Revision {
			return corestore.ErrRevisionConflict
		}
		_, err = w.AppendEvent(ctx, prepared)
		return err
	})
}
