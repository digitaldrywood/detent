package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *conversationTurnCoordinator) coordinatorMessages(ctx context.Context, filter conversationstore.MessageFilter) ([]conversationMessageRecord, error) {
	organization, err := c.service.lifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var messages []conversationMessageRecord
	refs := map[string]attachment.ArchiveReference{}
	err = c.service.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.ConversationByID(ctx, filter.ConversationID)
		if err != nil {
			return err
		}
		page, err := r.FilteredMessages(ctx, filter)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		attachments, err := owner.MessageAttachments(ctx, current.ProjectID, current.ID, ids)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			message := conversationMessageFromStore(item)
			if item.ContentReference != "" {
				object, err := r.ConversationContent(ctx, item.ContentReference)
				if err != nil {
					return err
				}
				refs[item.ContentReference] = object.Reference
			}
			for _, a := range attachments[item.ID] {
				message.Attachments = append(message.Attachments, conversationAttachmentFromStore(a))
			}
			references, err := r.MessageReferences(ctx, current.ID, item.ID)
			if err != nil {
				return err
			}
			for _, reference := range references {
				path := "/chat/c/" + url.PathEscape(reference.TargetID)
				if reference.Kind == conversation.ReferenceIssue {
					item, err := r.WorkItem(ctx, current.ProjectID, tracker.NativeWorkItemID(reference.TargetID))
					if err != nil {
						return err
					}
					org, err := r.AccessOrganization(ctx)
					if err != nil {
						return err
					}
					project, err := r.Project(ctx, current.ProjectID)
					if err != nil {
						return err
					}
					path = issueBrowserPath(org.Slug, project.Slug, int(item.Number))
				}
				message.References = append(message.References, conversationMessageReference{Kind: reference.Kind, ID: reference.TargetID, Label: reference.Label, URL: path})
			}
			messages = append(messages, message)
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	for i := range messages {
		if ref, ok := refs[messages[i].ContentID]; ok {
			var content conversationContent
			if err := c.service.server.readConversationObject(ctx, string(organization), ref, &content); err != nil {
				return nil, err
			}
			messages[i].Text, messages[i].Data = content.Text, content.Data
		}
	}
	slices.SortFunc(messages, func(a, b conversationMessageRecord) int {
		if a.Seq < b.Seq {
			return -1
		}
		if a.Seq > b.Seq {
			return 1
		}
		return 0
	})
	return messages, nil
}

func (c *conversationTurnCoordinator) authorizeRecord(ctx context.Context, scope nativeScope, record conversationRecord, write bool) error {
	return c.service.server.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := c.service.server.authorizeAccessScope(ctx, r, scope, write); err != nil {
			return err
		}
		if record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if record.ReadAudience != "" {
			audience, err := conversationReadAudienceRecord(ctx, r, scope.organization, scope.credential)
			if err != nil || audience != record.ReadAudience {
				return nativeNotFound()
			}
		}
		return authorizeConversationLinkedItemPG(ctx, r, scope, record)
	})
}

func (c *conversationTurnCoordinator) appendMessage(ctx context.Context, w tenantstore.Writer, record *conversationRecord, message *conversationMessageRecord, now time.Time) error {
	p := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan)
	prepared, ok := p.messages[message.ID]
	if ok {
		*message = prepared
	}
	created, err := w.CreateMessage(ctx, coordinatorStoredMessage(record.OrganizationID, *message))
	if err != nil {
		return err
	}
	message.Seq, message.Revision = created.Sequence, created.Revision
	if err := replaceCoordinatorReferences(ctx, w, record.OrganizationID, *message, now); err != nil {
		return err
	}
	record.MessageCount++
	if record.LastMessageAt == nil || record.LastMessageAt.Before(message.CreatedAt) {
		at := message.CreatedAt
		record.LastMessageAt = &at
	}
	wasSettled := record.Status == conversation.StatusSettled
	record.Status, record.SettledAt = conversation.StatusActive, nil
	if wasSettled {
		if err := c.saveConversation(ctx, w, record, now); err != nil {
			return err
		}
	}
	return appendCoordinatorEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventMessageAccepted, projectMessage(*message), now)
}

func (c *conversationTurnCoordinator) updateMessage(ctx context.Context, w tenantstore.Writer, message conversationMessageRecord, now time.Time) error {
	p := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan)
	if prepared, ok := p.messages[message.ID]; ok {
		message = prepared
	}
	current, err := w.Message(ctx, message.ConversationID, message.ID)
	if err != nil {
		return err
	}
	if current.Kind == conversation.MessageTool && current.ContentReference != "" {
		if err := w.PutToolRevision(ctx, conversationstore.ToolRevision{OrganizationID: current.OrganizationID, ConversationID: current.ConversationID, MessageID: current.ID, Revision: current.Revision, ContentReference: current.ContentReference, Tool: stringValue(current.ToolName), ItemID: stringValue(current.ToolItemID), Status: stringValue(current.ToolStatus)}); err != nil {
			return err
		}
	}
	row := coordinatorStoredMessage(current.OrganizationID, message)
	row.Revision, row.Sequence, row.UpdatedAt = current.Revision, current.Sequence, now
	updated, err := w.UpdateMessage(ctx, row, current.Revision)
	if err != nil {
		return err
	}
	message.Revision, message.UpdatedAt = updated.Revision, now
	if err := replaceCoordinatorReferences(ctx, w, current.OrganizationID, message, now); err != nil {
		return err
	}
	return appendCoordinatorEvent(ctx, w, current.OrganizationID, message.ConversationID, conversation.EventMessageUpdated, projectMessage(message), now)
}

func (c *conversationTurnCoordinator) saveConversation(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
	record.UpdatedAt = now
	stored, err := w.ConversationByID(ctx, record.ID)
	if err != nil {
		return err
	}
	record.EventSeq = stored.EventSequence
	plan := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan)
	raw, err := recordPayload(*record)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, ok := plan.prepared[string(record.OrganizationID)+"/"+artifact.Digest(content)]
	if !ok {
		return errors.New("coordinator record content was not prepared")
	}
	record.ContentID = contentArchiveID(string(record.OrganizationID), ref)
	current, err := w.UpdateConversation(ctx, conversationRecordToStore(*record), record.Revision)
	if err != nil {
		return err
	}
	*record = conversationRecordFromStore(current)
	return appendCoordinatorEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventConversationUpdated, projectConversation(plan.record), now)
}

func (c *conversationTurnCoordinator) updateExecution(ctx context.Context, w tenantstore.Writer, record *conversationRecord, execution conversation.Execution, now time.Time) error {
	execution.UpdatedAt = now
	record.Execution = execution
	if err := c.saveConversation(ctx, w, record, now); err != nil {
		return err
	}
	return appendCoordinatorEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventExecutionUpdated, projectExecution(execution), now)
}

func (c *conversationTurnCoordinator) appendDelta(ctx context.Context, w tenantstore.Writer, message *conversationMessageRecord, text string, now time.Time) error {
	offset := len(message.Text)
	plan := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan)
	prepared, ok := plan.messages[message.ID]
	if !ok {
		return errors.New("coordinator delta content was not prepared")
	}
	row, err := w.Message(ctx, message.ConversationID, message.ID)
	if err != nil {
		return err
	}
	update := coordinatorStoredMessage(row.OrganizationID, prepared)
	update.Revision, update.Sequence = row.Revision, row.Sequence
	row, err = w.UpdateMessage(ctx, update, row.Revision)
	if err != nil {
		return err
	}
	*message = prepared
	message.Revision = row.Revision
	return appendCoordinatorEvent(ctx, w, row.OrganizationID, message.ConversationID, conversation.EventMessageDelta, conversationDeltaFact{Text: text, Revision: row.Revision, MessageID: message.ID, Offset: int64(offset), Bytes: len(text)}, now)
}

func (c *conversationTurnCoordinator) setControlDelivery(ctx context.Context, w tenantstore.Writer, message *conversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError, now time.Time) error {
	message.Delivery = delivery
	if err := c.updateMessage(ctx, w, *message, now); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	command, found, err := w.Command(ctx, message.ConversationID, message.CommandKey)
	if err != nil || !found {
		return err
	}
	command.Receipt.Status, command.Receipt.Error, command.Receipt.UpdatedAt = delivery, failure, now
	if err := w.UpdateCommand(ctx, command); err != nil {
		return err
	}
	return appendCoordinatorEvent(ctx, w, command.OrganizationID, message.ConversationID, conversation.EventCommandReceipt, command.Receipt, now)
}

func (c *conversationTurnCoordinator) attachmentContent(ctx context.Context, id string) ([]byte, error) {
	organization, err := c.service.lifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var object conversationstore.ContentObject
	err = c.service.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, id)
		return err
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if c.service.store.contents == nil {
		return nil, conversationContentUnavailable()
	}
	return c.service.store.contents.ReadContent(ctx, string(organization), object.Reference)
}

func (c *conversationTurnCoordinator) modelChoiceGranted(ctx context.Context, r tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	d := c.service.server.database
	if d.plans() == nil || d.hostedOrganization != organization {
		return false, nil
	}
	entitlement, err := d.hostedEntitlementRecord(ctx, r, now)
	return slices.Contains(entitlement.Features, "model_choice"), err
}

func (c *conversationTurnCoordinator) modelChoices(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, now time.Time) ([]conversationModel, error) {
	owner, ok := r.(runnerstore.FleetReader)
	if !ok {
		return nil, conversationContentUnavailable()
	}
	runners, err := owner.FleetRecords(ctx, 1000)
	if err != nil {
		return nil, err
	}
	var models []conversationModel
	seen := map[string]bool{}
	for _, candidate := range runners {
		runner := candidate.Identity
		if candidate.CredentialRevoked || (candidate.CredentialExpiresAt != nil && !now.Before(*candidate.CredentialExpiresAt)) {
			continue
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, runner.RunnerID, project, now)
		if err != nil {
			return nil, err
		}
		if !allowed || runner.State != "active" {
			continue
		}
		for _, report := range candidate.Reports {
			if !reportFresh(report, now) {
				continue
			}
			for _, model := range report.Models {
				model = strings.TrimSpace(model)
				if model == "" || seen[model] {
					continue
				}
				seen[model] = true
				models = append(models, conversationModelFromReport(report, model))
			}
		}
	}
	s := c.service.server
	if s.hasLunaCoordinator() {
		models = slices.DeleteFunc(models, func(model conversationModel) bool { return model.ID == genkitbackend.Model })
		models = append(models, conversationModel{ID: genkitbackend.Model, Label: "Luna", Provider: "openai", Efforts: []string{"low", "medium"}, DefaultEffort: "low", BackendDefault: true})
	}
	if model := s.conversationDefaultModel(); model != "" && !slices.ContainsFunc(models, func(choice conversationModel) bool { return choice.ID == model }) {
		models = append(models, conversationModel{ID: model, Label: model})
	}
	return models, nil
}

func appendCoordinatorEvent(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, id string, kind conversation.EventType, _ any, now time.Time) error {
	plan := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan)
	if plan.eventCursor >= len(plan.events) {
		return errors.New("coordinator event was not prepared")
	}
	body := plan.events[plan.eventCursor]
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return err
	}
	ref, ok := plan.prepared[string(organization)+"/"+artifact.Digest(content)]
	if !ok {
		return errors.New("coordinator event content was not prepared")
	}
	_, err = w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: id, Event: conversation.Event{Type: kind, CreatedAt: now}, ContentReference: contentArchiveID(string(organization), ref)})
	if err == nil {
		plan.eventCursor++
	}
	return err
}

func coordinatorStoredMessage(organization tracker.OrganizationID, message conversationMessageRecord) conversationstore.Message {
	row := conversationMessageToStore(organization, message)
	var data queryMessageData
	if json.Unmarshal(message.Data, &data) == nil {
		row.ToolName, row.ToolItemID, row.ToolStatus = data.Tool, data.ItemID, data.Status
		row.OperatorResult = data.OperatorActionResult
		if data.OperatorAction != nil && data.OperatorAction.Action != nil {
			row.OperatorActionID, row.OperatorAgent = data.OperatorAction.Action.ID, data.OperatorAction.Action.Agent
		}
	}
	return row
}

func replaceCoordinatorReferences(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, message conversationMessageRecord, now time.Time) error {
	refs := make([]conversationstore.MessageReference, 0, len(message.References))
	for index, ref := range message.References {
		refs = append(refs, conversationstore.MessageReference{OrganizationID: organization, ConversationID: message.ConversationID, MessageID: message.ID, Position: int64(index), Kind: ref.Kind, TargetID: ref.ID, Label: ref.Label, CreatedAt: now})
	}
	return w.ReplaceMessageReferences(ctx, message.ConversationID, message.ID, refs, now)
}

func coordinatorMonthlyBudget(ctx context.Context, reader billingstore.Reader, project tracker.ProjectID, now time.Time) (budget.MonthlyDecision, error) {
	var input billingstore.MonthlyBudgetInput
	var err error
	input.Exposure = budget.CostExposure{LunaAPI: true}
	input.OrganizationPolicy, err = reader.BillingMonthlyPolicy(ctx, "")
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return budget.MonthlyDecision{}, err
	}
	input.ProjectPolicy, err = reader.BillingMonthlyPolicy(ctx, project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return budget.MonthlyDecision{}, err
	}
	if !input.OrganizationPolicy.Policy.Enabled && !input.ProjectPolicy.Policy.Enabled {
		return budget.MonthlyDecision{Allowed: true}, nil
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	input.OrganizationSpend, err = reader.BillingMonthlySpend(ctx, "", month)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	input.ProjectSpend, err = reader.BillingMonthlySpend(ctx, project, month)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	return billingstore.EvaluateMonthlyBudget(input)
}
