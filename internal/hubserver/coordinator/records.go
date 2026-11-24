package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RecordDeps struct {
	Service     *conversations.ConversationService
	ReadTenant  func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	WriteTenant func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
	ReadObject  func(context.Context, string, attachment.ArchiveReference, *conversations.ConversationContent) error
	Authorize   func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	Now         func() time.Time
	Cancel      func(string, string) bool
}

func (c RecordDeps) ReadMessages(ctx context.Context, filter conversationstore.MessageFilter) ([]conversations.ConversationMessageRecord, error) {
	organization, err := c.Service.LifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var messages []conversations.ConversationMessageRecord
	refs := map[string]attachment.ArchiveReference{}
	err = c.ReadTenant(ctx, organization, func(r tenantstore.Reader) error {
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
			return hubapi.FromFeatureError(conversations.ConversationContentUnavailable())
		}
		attachments, err := owner.MessageAttachments(ctx, current.ProjectID, current.ID, ids)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			message := conversations.ConversationMessageFromStore(item)
			if item.ContentReference != "" {
				object, err := r.ConversationContent(ctx, item.ContentReference)
				if err != nil {
					return err
				}
				refs[item.ContentReference] = object.Reference
			}
			for _, a := range attachments[item.ID] {
				message.Attachments = append(message.Attachments, conversations.ConversationAttachmentFromStore(a))
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
					path = hubrecords.IssueBrowserPath(org.Slug, project.Slug, int(item.Number))
				}
				message.References = append(message.References, conversations.ConversationMessageReference{Kind: reference.Kind, ID: reference.TargetID, Label: reference.Label, URL: path})
			}
			messages = append(messages, message)
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.FromFeatureError(conversations.TranslateConversationError(err))
	}
	for i := range messages {
		if ref, ok := refs[messages[i].ContentID]; ok {
			var content conversations.ConversationContent
			if err := c.ReadObject(ctx, string(organization), ref, &content); err != nil {
				return nil, err
			}
			messages[i].Text, messages[i].Data = content.Text, content.Data
		}
	}
	slices.SortFunc(messages, func(a, b conversations.ConversationMessageRecord) int {
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

func (c RecordDeps) AuthorizeRecord(ctx context.Context, scope hubscope.NativeScope, record conversations.ConversationRecord, write bool) error {
	if record.OrganizationID != scope.Organization || record.ProjectID != scope.Project {
		return hubapi.NativeNotFound()
	}
	return c.ReadTenant(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := c.Authorize(ctx, r, scope, write); err != nil {
			return err
		}
		if record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.Credential.ID {
			return hubapi.NativeNotFound()
		}
		if record.ReadAudience != "" {
			audience, err := conversations.ConversationReadAudienceRecord(ctx, r, scope.Organization, hubscope.AuthCredential(scope.Credential))
			if err != nil || audience != record.ReadAudience {
				return hubapi.NativeNotFound()
			}
		}
		return hubapi.FromFeatureError(conversations.AuthorizeConversationLinkedItemPG(ctx, r, conversations.ScopeInput(scope), record))
	})
}

func AppendMessage(ctx context.Context, w tenantstore.Writer, record *conversations.ConversationRecord, message *conversations.ConversationMessageRecord, now time.Time) error {
	p, err := coordinatorPublicationPlan(ctx)
	if err != nil {
		return err
	}
	prepared, ok := p.Messages[message.ID]
	if ok {
		*message = prepared
	}
	created, err := w.CreateMessage(ctx, coordinatorStoredMessage(record.OrganizationID, *message))
	if err != nil {
		return err
	}
	message.Seq, message.Revision = created.Sequence, created.Revision
	references, err := conversations.ResolveMessageReferences(ctx, w, *record, message.Text)
	if err != nil {
		return err
	}
	message.References = references
	if err := conversations.ReplaceMessageReferences(ctx, w, record.OrganizationID, *message, now); err != nil {
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
		if err := SaveConversation(ctx, w, record, now); err != nil {
			return err
		}
	}
	return AppendEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventMessageAccepted, conversations.ProjectMessage(*message), now)
}

func UpdateMessage(ctx context.Context, w tenantstore.Writer, message conversations.ConversationMessageRecord, now time.Time) error {
	p, err := coordinatorPublicationPlan(ctx)
	if err != nil {
		return err
	}
	if prepared, ok := p.Messages[message.ID]; ok {
		message = prepared
	}
	current, err := w.Message(ctx, message.ConversationID, message.ID)
	if err != nil {
		return err
	}
	if current.Kind == conversation.MessageTool && current.ContentReference != "" {
		if err := w.PutToolRevision(ctx, conversationstore.ToolRevision{OrganizationID: current.OrganizationID, ConversationID: current.ConversationID, MessageID: current.ID, Revision: current.Revision, ContentReference: current.ContentReference, Tool: hubapi.StringValue(current.ToolName), ItemID: hubapi.StringValue(current.ToolItemID), Status: hubapi.StringValue(current.ToolStatus)}); err != nil {
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
	if message.Delivery.Terminal() {
		stored, err := w.ConversationByID(ctx, message.ConversationID)
		if err != nil {
			return err
		}
		message.References, err = conversations.ResolveMessageReferences(ctx, w, conversations.ConversationRecordFromStore(stored), message.Text)
		if err != nil {
			return err
		}
	}
	if err := conversations.ReplaceMessageReferences(ctx, w, current.OrganizationID, message, now); err != nil {
		return err
	}
	return AppendEvent(ctx, w, current.OrganizationID, message.ConversationID, conversation.EventMessageUpdated, conversations.ProjectMessage(message), now)
}

func SaveConversation(ctx context.Context, w tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
	record.UpdatedAt = now
	stored, err := w.ConversationByID(ctx, record.ID)
	if err != nil {
		return err
	}
	record.EventSeq = stored.EventSequence
	plan, err := coordinatorPublicationPlan(ctx)
	if err != nil {
		return err
	}
	raw, err := conversations.RecordPayload(*record)
	if err != nil {
		return hubapi.FromFeatureError(err)
	}
	content, err := conversations.MessageContent(conversations.ConversationMessageRecord{Data: raw})
	if err != nil {
		return hubapi.FromFeatureError(err)
	}
	ref, ok := plan.Prepared[string(record.OrganizationID)+"/"+artifact.Digest(content)]
	if !ok {
		return errors.New("coordinator record content was not prepared")
	}
	record.ContentID = conversations.ContentArchiveID(string(record.OrganizationID), ref)
	current, err := w.UpdateConversation(ctx, conversations.ConversationRecordToStore(*record), record.Revision)
	if err != nil {
		return err
	}
	*record = conversations.ConversationRecordFromStore(current)
	return AppendEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventConversationUpdated, conversations.ProjectConversation(plan.Record), now)
}

func UpdateExecution(ctx context.Context, w tenantstore.Writer, record *conversations.ConversationRecord, execution conversation.Execution, now time.Time) error {
	execution.UpdatedAt = now
	record.Execution = execution
	if err := SaveConversation(ctx, w, record, now); err != nil {
		return err
	}
	return AppendEvent(ctx, w, record.OrganizationID, record.ID, conversation.EventExecutionUpdated, conversations.ProjectExecution(execution), now)
}

func AppendDelta(ctx context.Context, w tenantstore.Writer, message *conversations.ConversationMessageRecord, text string, now time.Time) error {
	offset := len(message.Text)
	plan, err := coordinatorPublicationPlan(ctx)
	if err != nil {
		return err
	}
	prepared, ok := plan.Messages[message.ID]
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
	return AppendEvent(ctx, w, row.OrganizationID, message.ConversationID, conversation.EventMessageDelta, conversations.ConversationDeltaFact{Text: text, Revision: row.Revision, MessageID: message.ID, Offset: int64(offset), Bytes: len(text)}, now)
}

func SetControlDelivery(ctx context.Context, w tenantstore.Writer, message *conversations.ConversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError, now time.Time) error {
	message.Delivery = delivery
	if err := UpdateMessage(ctx, w, *message, now); err != nil {
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
	return AppendEvent(ctx, w, command.OrganizationID, message.ConversationID, conversation.EventCommandReceipt, command.Receipt, now)
}

func (c RecordDeps) AttachmentContent(ctx context.Context, id string) ([]byte, error) {
	organization, err := c.Service.LifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var object conversationstore.ContentObject
	err = c.ReadTenant(ctx, organization, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, id)
		return err
	})
	if err != nil {
		return nil, hubapi.FromFeatureError(conversations.TranslateConversationError(err))
	}
	if c.Service.Store.Contents == nil {
		return nil, hubapi.FromFeatureError(conversations.ConversationContentUnavailable())
	}
	return c.Service.Store.Contents.ReadContent(ctx, string(organization), object.Reference)
}

func AppendEvent(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, id string, kind conversation.EventType, _ any, now time.Time) error {
	plan, err := coordinatorPublicationPlan(ctx)
	if err != nil {
		return err
	}
	if plan.EventCursor >= len(plan.Events) {
		return errors.New("coordinator event was not prepared")
	}
	body := plan.Events[plan.EventCursor]
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	content, err := conversations.MessageContent(conversations.ConversationMessageRecord{Data: raw})
	if err != nil {
		return hubapi.FromFeatureError(err)
	}
	ref, ok := plan.Prepared[string(organization)+"/"+artifact.Digest(content)]
	if !ok {
		return errors.New("coordinator event content was not prepared")
	}
	_, err = w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: id, Event: conversation.Event{Type: kind, CreatedAt: now}, ContentReference: conversations.ContentArchiveID(string(organization), ref)})
	if err == nil {
		plan.EventCursor++
	}
	return err
}

func coordinatorStoredMessage(organization tracker.OrganizationID, message conversations.ConversationMessageRecord) conversationstore.Message {
	row := conversations.ConversationMessageToStore(organization, message)
	var data conversations.QueryMessageData
	if json.Unmarshal(message.Data, &data) == nil {
		row.ToolName, row.ToolItemID, row.ToolStatus = data.Tool, data.ItemID, data.Status
		row.OperatorResult = data.OperatorActionResult
		if data.OperatorAction != nil && data.OperatorAction.Action != nil {
			row.OperatorActionID, row.OperatorAgent = data.OperatorAction.Action.ID, data.OperatorAction.Action.Agent
		}
	}
	return row
}

func MonthlyBudget(ctx context.Context, reader billingstore.Reader, project tracker.ProjectID, now time.Time) (budget.MonthlyDecision, error) {
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

func coordinatorPublicationPlan(ctx context.Context) (*conversations.ConversationPublicationPlan, error) {
	plan, ok := ctx.Value(conversations.ConversationPublicationKey{}).(*conversations.ConversationPublicationPlan)
	if !ok || plan == nil {
		return nil, errors.New("coordinator publication plan was not prepared")
	}
	return plan, nil
}

func (c RecordDeps) DeliverCommands(ctx context.Context, id string, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	organization, err := c.Service.LifecycleOrganization()
	if err != nil {
		return 0, err
	}
	var record conversationstore.Conversation
	var page conversationstore.CommandPage
	if err := c.ReadTenant(ctx, organization, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ConversationByID(ctx, id)
		if err != nil {
			return err
		}
		page, err = r.PendingCoordinatorCommands(ctx, id, "", limit)
		return err
	}); err != nil {
		return 0, err
	}
	for _, command := range page.Items {
		if err := ctx.Err(); err != nil {
			return len(page.Items), err
		}
		delivered := c.Cancel(id, command.TargetTurnID)
		if !delivered && command.TargetTurnID != "" {
			if err := c.ReadTenant(ctx, organization, func(r tenantstore.Reader) error {
				target, err := r.Message(ctx, id, command.TargetTurnID)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				delivered = target.Role == conversation.RoleAssistant && target.Delivery == conversation.DeliveryInterrupted
				return nil
			}); err != nil {
				return len(page.Items), err
			}
		}
		receipt := command.Receipt
		receipt.Status = conversation.DeliveryDelivered
		receipt.Error = nil
		if !delivered {
			receipt.Status = conversation.DeliveryRejected
			receipt.Error = &conversation.ReceiptError{Code: "no_active_turn", Message: "The intended coordinator turn is no longer running"}
		}
		now := c.Now()
		receipt.UpdatedAt = now
		plan := conversations.ConversationPublicationPlan{Service: c.Service, Record: conversations.ConversationRecordFromStore(record), Now: now, Coordinator: true}
		if err := plan.RecordReceipt(ctx, receipt); err != nil {
			return len(page.Items), err
		}
		prepared := plan.Context(ctx)
		if err := c.WriteTenant(prepared, organization, func(w tenantstore.Writer) error {
			if _, err := w.LockConversation(prepared, record.ProjectID, id); err != nil {
				return err
			}
			stored, found, err := w.Command(prepared, id, command.Key)
			if err != nil {
				return err
			}
			if !found {
				return corestore.ErrNotFound
			}
			if stored.Kind != conversation.CommandCancel || stored.TargetTurnID != command.TargetTurnID || stored.RequestHash != command.RequestHash {
				return corestore.ErrRevisionConflict
			}
			if stored.Receipt.Status != conversation.DeliverySaved {
				return nil
			}
			if err := conversations.RegisterPreparedConversationObjects(prepared, w, organization, plan.Prepared, now); err != nil {
				return err
			}
			stored.Receipt, stored.UpdatedAt = receipt, now
			if err := w.UpdateCommand(prepared, stored); err != nil {
				return err
			}
			return AppendEvent(prepared, w, organization, id, conversation.EventCommandReceipt, receipt, now)
		}); err != nil {
			return len(page.Items), err
		}
		c.Service.Broker.Notify(id)
	}
	return len(page.Items), nil
}
