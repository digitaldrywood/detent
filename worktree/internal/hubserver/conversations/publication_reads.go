package conversations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *ConversationService) ReadPublicationConversation(ctx context.Context, id string) (ConversationRecord, error) {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return ConversationRecord{}, err
	}
	return c.ReadPublicationConversationForOrganization(ctx, organization, id)
}

func (c *ConversationService) ReadPublicationConversationForOrganization(ctx context.Context, organization tracker.OrganizationID, id string) (ConversationRecord, error) {
	var record ConversationRecord
	var ref attachment.ArchiveReference
	err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.ConversationByID(ctx, id)
		if err != nil {
			return err
		}
		record = ConversationRecordFromStore(stored)
		if record.ContentID != "" && (record.Execution.Status == "" || record.Preferences == (conversation.Preferences{})) {
			content, err := r.ConversationContent(ctx, record.ContentID)
			if err != nil {
				return err
			}
			ref = content.Reference
		}
		if record.WorkItemID != "" {
			item, err := r.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
			if err != nil {
				return err
			}
			project, err := r.Project(ctx, record.ProjectID)
			if err != nil {
				return err
			}
			record.WorkItem = &ConversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
			if item.WorkflowStateName != nil {
				record.WorkItem.Lane = *item.WorkflowStateName
			}
		}
		return nil
	})
	if err != nil {
		return record, TranslateConversationError(err)
	}
	if record.ContentID != "" && (record.Execution.Status == "" || record.Preferences == (conversation.Preferences{})) {
		var content ConversationContent
		if err := c.Server.ReadConversationObject(ctx, string(organization), ref, &content); err != nil {
			return record, err
		}
		var payload ConversationRecordContent
		if err := json.Unmarshal(content.Data, &payload); err != nil {
			return record, err
		}
		if record.Execution.Status == "" {
			record.Execution = payload.Execution
		}
		if record.Preferences == (conversation.Preferences{}) {
			record.Preferences = payload.Preferences.Normalized()
		}
	}
	return record, nil
}

func (c *ConversationService) ReadPublicationMessage(ctx context.Context, organization tracker.OrganizationID, id, messageID string) (ConversationMessageRecord, error) {
	var message ConversationMessageRecord
	var ref attachment.ArchiveReference
	err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Message(ctx, id, messageID)
		if err != nil {
			return err
		}
		message = ConversationMessageFromStore(stored)
		if message.ContentID != "" {
			content, err := r.ConversationContent(ctx, message.ContentID)
			if err != nil {
				return err
			}
			ref = content.Reference
		}
		return nil
	})
	if err != nil {
		return message, TranslateConversationError(err)
	}
	if message.ContentID != "" {
		var content ConversationContent
		if err := c.Server.ReadConversationObject(ctx, string(organization), ref, &content); err != nil {
			return message, err
		}
		message.Text, message.Data = content.Text, content.Data
	}
	return message, nil
}
