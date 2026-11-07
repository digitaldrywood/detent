package hubserver

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) WorkItemPageMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "work-item-pages", Bytes: hubmaintenance.ByteLimit, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		for range budget.Items {
			worked, err := s.MaintainWorkItemPage(ctx, tracker.OrganizationID(tenant.Organization))
			if err != nil || !worked {
				return err
			}
		}
		return nil
	}}
}

func (s *Service) MaintainWorkItemPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), hubmaintenance.ByteLimit); err != nil {
		return false, err
	}
	if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
		return false, errors.New("maintenance requires byte-limited payload storage")
	}
	complete := false
	if err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		backfill, ok := w.(viewstore.WorkItemPageBackfiller)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		complete, err = backfill.BackfillWorkItemPages(ctx)
		return err
	}); err != nil {
		return false, err
	}
	if !complete {
		return true, nil
	}
	ctx = context.WithValue(ctx, detailBatchContextKey{}, nil)
	ctx = context.WithValue(ctx, changeContentKey{}, nil)
	ctx = context.WithValue(ctx, runDetailContextKey{}, nil)
	var candidate viewstore.PageCandidate
	var item corestore.WorkItem
	var comment corestore.Comment
	var version eventstore.CollaborationVersion
	var browserPath string
	var relations corestore.RelationshipPage
	missing := false
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.WorkItemPageReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		candidate, err = reader.NextWorkItemPageCandidate(ctx)
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, candidate.ProjectID)
		if errors.Is(err, corestore.ErrNotFound) || err == nil && project.DeletedAt != nil {
			missing = true
			return nil
		}
		if err != nil {
			return err
		}
		if candidate.Kind == "relationships" {
			if candidate.Cleaning {
				return nil
			}
			relations, err = r.Relationships(ctx, candidate.ProjectID, candidate.WorkItemID, candidate.After, corestore.MaxPageSize)
			return err
		}
		if candidate.Kind == "item" {
			item, err = r.WorkItem(ctx, candidate.ProjectID, candidate.WorkItemID)
			if err == nil {
				organization, readErr := r.AccessOrganization(ctx)
				if readErr != nil {
					return readErr
				}
				browserPath = "/" + organization.Slug + "/" + project.Slug + "/issues/" + strconv.FormatInt(item.Number, 10)
			}
		} else {
			comment, err = r.Comment(ctx, candidate.ProjectID, candidate.WorkItemID, candidate.RecordID)
		}
		if errors.Is(err, corestore.ErrNotFound) {
			missing = true
			return nil
		}
		if err != nil {
			return err
		}
		revision := item.Revision
		if candidate.Kind == "comment" {
			revision = comment.Revision
		}
		version, err = r.CollaborationVersion(ctx, candidate.RecordID, revision)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) && candidate.Generation == 0 {
		return false, nil
	}
	if candidate.Generation != 0 {
		cursorErr := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			writer, ok := w.(viewstore.WorkItemPageWriter)
			if !ok {
				return workItemStorageUnavailable()
			}
			return writer.AdvanceWorkItemPageCursor(ctx, candidate)
		})
		if err != nil || cursorErr != nil {
			return false, errors.Join(err, cursorErr)
		}
	}
	if err != nil {
		return false, err
	}

	if candidate.Kind == "relationships" {
		records := make([]viewstore.RelationshipPageRecord, 0, len(relations.Items))
		for _, relation := range relations.Items {
			browserURL := ""
			if relation.OrganizationSlug != "" && relation.ProjectSlug != "" {
				browserURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: issueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
			}
			records = append(records, viewstore.RelationshipPageRecord{Relationship: relation, WebURL: browserURL, Identifier: string(relation.RelatedProjectID) + "#" + strconv.Itoa(relation.Number)})
		}
		err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			writer, ok := w.(viewstore.RelationshipPageWriter)
			if !ok {
				return workItemStorageUnavailable()
			}
			return writer.PublishRelationshipPage(ctx, candidate, records, relations.Next)
		})
		return err == nil, err
	}
	var itemPage *viewstore.ItemPageRecord
	var commentPage *viewstore.CommentPageRecord
	if !missing {
		if candidate.Kind == "item" {
			value, err := s.hydrateWorkItemRecord(ctx, item, version)
			if err != nil {
				return false, err
			}
			value.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: browserPath}).WebURL
			if value.LinkedSource != nil {
				value.LinkedSource.Snapshot = nil
			}
			itemPage = &viewstore.ItemPageRecord{Value: value, Generation: candidate.Generation, SourceAt: candidate.SourceAt, ProjectedAt: s.config.now()}
		} else {
			value, err := s.hydrateCommentRecord(ctx, comment, version)
			if err != nil {
				return false, err
			}
			commentPage = &viewstore.CommentPageRecord{Value: value, Generation: candidate.Generation, SourceAt: candidate.SourceAt, ProjectedAt: s.config.now()}
		}
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(viewstore.WorkItemPageWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		return writer.PublishWorkItemPage(ctx, candidate, itemPage, commentPage)
	})
	if err != nil {
		return false, err
	}
	if s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project work item page", slog.String("organization_id", string(organization)), slog.String("kind", candidate.Kind), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return true, nil
}
