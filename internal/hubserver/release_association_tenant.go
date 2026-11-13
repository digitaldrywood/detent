package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) associateReleasePickTenant(ctx context.Context, scope hubscope.NativeScope, item, operation string, request tracker.AssociateReleasePick) (json.RawMessage, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.releaseAssociationReceipt(ctx, scope, operation, request.IdempotencyKey, hash); err != nil || found {
		return raw, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	existing, refreshing, err := s.readReleaseDeliveryRecord(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	if refreshing && existing.ReleasePickInput == request.ReleasePickInput {
		return s.replayReleaseAssociation(ctx, scope, item, operation, request, hash, existing)
	}
	check, err := s.prepareReleaseDeliveryCheck(ctx, scope, item, request, now, refreshing)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	delivery := tracker.ReleaseDelivery{ReleasePickInput: request.ReleasePickInput, Actor: scope.Actor(), AssociatedAt: now}
	unchanged := check.found && check.existing.ReleasePickInput == request.ReleasePickInput
	if unchanged {
		delivery = check.existing
	}
	var change tracker.ChangeRequest
	var changeRow eventstore.Change
	var transition *changeWorkflowTransition
	plan := hubrecords.WorkItemRecordPlan{Current: check.current, Project: check.project, Issue: check.issue, EventType: "issue.edited"}
	var fields hubrecords.WorkItemRecordFields
	var version eventstore.CollaborationVersion
	var events []eventstore.Event
	var search searchstore.Document
	if !unchanged {
		if check.found {
			change, err = s.readChangeRecord(ctx, scope, item, check.existing.ChangeID)
			if err != nil {
				return nil, err
			}
			transition, err = s.prepareChangeWorkflowTransition(ctx, scope, change, check.approval, true, now)
			if err != nil {
				return nil, err
			}
			change.Revision++
			change.CurrentVersion, change.UpdatedAt = "", now
		} else {
			if err := approveReleaseDeliveryContract(&plan.Issue, check.approval.Policy, now); err != nil {
				return nil, err
			}
			change = tracker.ChangeRequest{ID: hubcontent.PayloadNativeID(ctx, "change"), OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), LinkedIssues: []tracker.NativeWorkItemID{tracker.NativeWorkItemID(item)}, Title: plan.Issue.Title, Revision: 1, CreatedAt: now, UpdatedAt: now}
		}
		delivery.ChangeID = change.ID
		raw, err := json.Marshal(change)
		if err != nil {
			return nil, err
		}
		changeRow, err = content.PrepareChange(ctx, scope.Organization, raw)
		if err != nil {
			return nil, err
		}
		if check.found {
			changeRow.Revision--
		}
		plan.Issue.ReleaseDelivery = &delivery
		plan.Issue.Revision++
		plan.Issue.UpdatedAt, plan.Issue.LastActivityAt = now, now
		search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), ID: "issue:" + item, SourceID: item, Kind: "issue", Scope: "issue", Revision: int64(plan.Issue.Revision), UpdatedAt: now}, searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels})
		if err != nil {
			return nil, err
		}
		fields, err = s.prepareWorkItemFields(ctx, scope.Organization, plan)
		if err != nil {
			return nil, err
		}
		raw, err = json.Marshal(s.nativeIssueResponse(plan.Issue))
		if err != nil {
			return nil, err
		}
		version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), RecordID: item, Revision: int64(plan.Issue.Revision)}, raw)
		if err != nil {
			return nil, err
		}
		kinds := []string{"issue.edited"}
		if !check.found {
			kinds = []string{"change.created", "issue.edited"}
		}
		for _, kind := range kinds {
			raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(item), Type: kind, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, Change: &tracker.NativeChangeReference{ChangeID: delivery.ChangeID, HeadSHA: delivery.HeadSHA}}})
			if err != nil {
				return nil, err
			}
			event, err := content.PrepareEvent(ctx, scope.Organization, raw)
			if err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		if transition != nil && transition.eligible {
			transition.plan.Issue.ReleaseDelivery = &delivery
			transition.plan.Issue.Revision++
			transition.version.Revision++
			raw, err := json.Marshal(s.nativeIssueResponse(transition.plan.Issue))
			if err != nil {
				return nil, err
			}
			transition.version, err = content.PrepareCollaborationVersion(ctx, transition.version, raw)
			if err != nil {
				return nil, err
			}
			transition.search.Revision++
			raw, err = json.Marshal(tracker.CollaborationEvent{ID: transition.event.ID, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(item), Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: transition.plan.Issue.Revision, FromState: plan.Issue.State, ToState: transition.plan.Issue.State, Reason: "user_requested"}})
			if err != nil {
				return nil, err
			}
			transition.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
			if err != nil {
				return nil, err
			}

		}
	}
	raw, err := json.Marshal(delivery)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, raw)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
			return err
		}
		if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
			if prior.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			return corestore.ErrSerializationFailure
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if _, err := w.LockWorkItems(ctx, []corestore.WorkItemReference{{ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item)}, {ProjectID: scope.Project, WorkItemID: request.SourceIssueID}}); err != nil {
			return err
		}
		if err := s.recheckReleaseDelivery(ctx, w, scope, check); err != nil {
			return err
		}
		if s.database.plansService().Plans() != nil {
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", s.config.now())); err != nil {
				return err
			}
		}
		writer, ok := w.(eventstore.ReleaseDeliveryWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if !unchanged {
			if transition != nil {
				if _, err := s.lockChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
				}
			}
			if check.found {
				current, err := w.LockChange(ctx, changeRow.ID)
				if err != nil {
					return err
				}
				if current.Revision != changeRow.Revision {
					return corestore.ErrRevisionConflict
				}
				changeRow.EvidenceSequence = current.EvidenceSequence
				if _, err := w.UpdateChange(ctx, changeRow); err != nil {
					return err
				}
			} else {
				if _, err := w.CreateChange(ctx, changeRow); err != nil {
					return err
				}
				if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), ChangeID: change.ID}); err != nil {
					return err
				}
			}
			if err := writer.PutReleaseDelivery(ctx, eventstore.ReleaseDelivery{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), RepositoryID: check.deliveryRow.RepositoryID, ReleaseDelivery: delivery}); err != nil {
				return err
			}
			if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, fields); err != nil {
				return err
			}
			if err := w.PutSearch(ctx, search, plan.Current.Revision); err != nil {
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if err := w.PutSearch(ctx, search, 0); err != nil {
					return err
				}
			}
			if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
				return err
			}
			for _, event := range events {
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
			}
			if transition != nil && transition.eligible {
				transition.plan.Current.Revision++
				if err := s.applyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
				}
			}
		}
		_, err := w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
		if replay, found, replayErr := s.releaseAssociationReceipt(ctx, scope, operation, request.IdempotencyKey, hash); replayErr != nil || found {
			return replay, replayErr
		}
	}
	if err != nil {
		return nil, releaseAssociationError(err)
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return raw, nil
}
