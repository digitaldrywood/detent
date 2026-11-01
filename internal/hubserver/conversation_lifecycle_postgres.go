package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const conversationLifecycleBatchSize = 200

func (c *conversationService) lifecycleCursor(kind string) string {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return c.lifecycleCursors[kind]
}

func (c *conversationService) advanceLifecycleCursor(kind, next string) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.lifecycleCursors == nil {
		c.lifecycleCursors = make(map[string]string)
	}
	c.lifecycleCursors[kind] = next
}

func (c *conversationService) lifecycleOrganization() (tracker.OrganizationID, error) {
	if !c.server.conversationRecordsSelected() {
		return "", errors.New("conversation tenant repository is required for PostgreSQL lifecycle work")
	}
	organization := strings.TrimSpace(c.server.appOrganizationID())
	if organization == "" {
		return "", errors.New("conversation service organization is required for PostgreSQL lifecycle work")
	}
	return tracker.OrganizationID(organization), nil
}

func (c *conversationService) normalizeAfterRestartPG(ctx context.Context, organization tracker.OrganizationID, now time.Time) (conversationNormalizeSummary, error) {
	var summary conversationNormalizeSummary
	after := ""
	for {
		var page conversationstore.ConversationPage
		if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleRestart, AfterID: after, Limit: conversationLifecycleBatchSize})
			return err
		}); err != nil {
			return summary, fmt.Errorf("list conversations for PostgreSQL restart normalization: %w", err)
		}
		for _, candidate := range page.Items {
			for {
				batch, more, err := c.normalizeRestartConversationBatchPG(ctx, organization, candidate, now)
				if err != nil {
					return summary, fmt.Errorf("normalize PostgreSQL conversation after restart: %w", err)
				}
				summary.Conversations += batch.Conversations
				summary.Messages += batch.Messages
				summary.Questions += batch.Questions
				summary.Receipts += batch.Receipts
				if !more {
					break
				}
			}
		}
		if page.Next == "" {
			return summary, nil
		}
		after = page.Next
	}
}

func (c *conversationService) normalizeRestartConversationBatchPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation, now time.Time) (conversationNormalizeSummary, bool, error) {
	var summary conversationNormalizeSummary
	more := false
	err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		stored, err := writer.LockConversation(ctx, candidate.ProjectID, candidate.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if stored.WorkItemID != "" {
			return nil
		}
		messages, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{
			ConversationID: stored.ID,
			Deliveries:     []conversation.Delivery{conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding},
			Limit:          conversationLifecycleBatchSize,
		})
		if err != nil {
			return err
		}
		for _, message := range messages.Items {
			message.Delivery, message.UpdatedAt = conversation.NormalizeDelivery(message.Delivery), now
			if _, err := writer.UpdateMessage(ctx, message, message.Revision); err != nil {
				return err
			}
			summary.Messages++
		}
		more = more || messages.Next != 0

		afterQuestion := ""
		questions, err := writer.RestartQuestions(ctx, stored.ID, afterQuestion, conversationLifecycleBatchSize)
		if err != nil {
			return err
		}
		for _, question := range questions.Items {
			question.Status, question.UpdatedAt = conversation.NormalizeQuestion(question.Status), now
			if err := writer.UpsertQuestion(ctx, question); err != nil {
				return err
			}
			summary.Questions++
		}
		more = more || questions.Next != ""

		commands, err := writer.PendingCommands(ctx, stored.ID, "", conversationLifecycleBatchSize)
		if err != nil {
			return err
		}
		for _, command := range commands.Items {
			command.Receipt.Status, command.Receipt.UpdatedAt = conversation.DeliveryUnknown, now
			if err := writer.UpdateCommand(ctx, command); err != nil {
				return err
			}
			summary.Receipts++
		}
		more = more || commands.Next != ""

		execution := stored.Execution
		normalized := conversation.NormalizeExecutionValue(execution)
		if normalized.Status != execution.Status {
			normalized.UpdatedAt = now
			stored.Execution, stored.UpdatedAt = normalized, now
			if _, err := writer.UpdateConversation(ctx, stored, stored.Revision); err != nil {
				return err
			}
			summary.Conversations++
		}
		return nil
	})
	return summary, more, err
}

func (c *conversationService) wakePendingPG(ctx context.Context, organization tracker.OrganizationID) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	const cursor = "pending"
	after := c.lifecycleCursor(cursor)
	coordinator, controls := 0, 0
	var page conversationstore.PendingConversationPage
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.PendingConversations(ctx, after, conversationLifecycleBatchSize)
		return err
	}); err != nil {
		return fmt.Errorf("list PostgreSQL conversations with pending messages: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	for _, pending := range page.Items {
		if pending.Linked {
			c.controls.Wake(pending.ID)
			controls++
			continue
		}
		if c.coordinator.Available() {
			c.coordinator.Wake(pending.ID)
			coordinator++
		}
	}
	c.logger.Info("conversation.wake_pending", "coordinator", coordinator, "controls", controls)
	return nil
}

func (c *conversationService) reconcileExecutionsPG(ctx context.Context, organization tracker.OrganizationID) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	const cursor = "execution"
	after := c.lifecycleCursor(cursor)
	var errs []error
	var page conversationstore.ConversationPage
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleExecution, AfterID: after, Limit: conversationLifecycleBatchSize})
		return err
	}); err != nil {
		return fmt.Errorf("list PostgreSQL conversation executions: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	for _, candidate := range page.Items {
		changed, err := c.reconcileExecutionPG(ctx, organization, candidate)
		if err != nil {
			errs = append(errs, fmt.Errorf("conversation %s: %w", candidate.ID, err))
			continue
		}
		if changed {
			var record conversationRecord
			if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
				var err error
				record, err = readConversationRecordFromTenant(ctx, reader, candidate.ProjectID, candidate.ID)
				return err
			}); err != nil {
				errs = append(errs, fmt.Errorf("read reconciled PostgreSQL conversation %s: %w", candidate.ID, err))
				continue
			}
			c.committed(record)
		}
	}
	return errors.Join(errs...)
}

func (c *conversationService) reconcileExecutionPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation) (bool, error) {
	changed := false
	now := c.server.config.now()
	err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		stored, err := writer.LockConversation(ctx, candidate.ProjectID, candidate.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		record := conversationRecordFromStore(stored)
		if stored.WorkItemID == "" || record.Execution.Owner.LeaseID == "" || record.Execution.Owner.AttemptID == "" || (record.Execution.Status.Terminal() && record.Execution.Settled) {
			return nil
		}
		current, err := conversationLeaseCurrentPG(ctx, writer, record, now)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
		status := record.Execution.Status
		if !status.Terminal() {
			status = conversation.ExecutionInterrupted
		}
		processed, moreMessages, err := settlePGWorkerMessagesBatch(ctx, writer, &record, status, now, conversationLifecycleBatchSize)
		if err != nil {
			return err
		}
		moreQuestions := false
		remaining := conversationLifecycleBatchSize - processed
		if remaining > 0 {
			questions, err := writer.OutstandingQuestions(ctx, record.ID, record.Execution.Owner.AttemptID, "", "", remaining)
			if err != nil {
				return err
			}
			for _, question := range questions.Items {
				question.Status, question.UpdatedAt = conversation.QuestionExpired, now
				if err := writer.UpsertQuestion(ctx, question); err != nil {
					return err
				}
				if err := appendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventQuestionUpdated, question.Question, now); err != nil {
					return err
				}
			}
			moreQuestions = questions.Next != ""
		}
		record.Execution.Status, record.Execution.Error = status, ""
		record.Execution.Settled = !moreMessages && !moreQuestions
		record.Execution.Capabilities, record.Execution.UpdatedAt = conversation.Capabilities{}, now
		currentRecord, err := writer.Conversation(ctx, record.ProjectID, record.ID)
		if err != nil {
			return err
		}
		currentRecord.Execution, currentRecord.UpdatedAt = record.Execution, now
		updated, err := writer.UpdateConversation(ctx, currentRecord, currentRecord.Revision)
		if err != nil {
			return err
		}
		updatedRecord := conversationRecordFromStore(updated)
		updatedRecord.WorkItem = record.WorkItem
		if err := appendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventConversationUpdated, projectConversation(updatedRecord), now); err != nil {
			return err
		}
		if err := appendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventExecutionUpdated, projectExecution(updated.Execution), now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func conversationLeaseCurrentPG(ctx context.Context, reader tenantstore.Reader, record conversationRecord, now time.Time) (bool, error) {
	lease, err := reader.Lease(ctx, record.Execution.Owner.LeaseID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	item, err := reader.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attempt, err := reader.Attempt(ctx, record.Execution.Owner.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owner := record.Execution.Owner
	return lease.OrganizationID == record.OrganizationID && lease.IssueID == item.ID && lease.FencingToken == owner.FencingToken && lease.ReleasedAt == nil && lease.ExpiresAt.After(now) && !lease.RenewedAt.After(now) &&
		attempt.OrganizationID == record.OrganizationID && attempt.IssueID == item.ID && attempt.LeaseID == lease.LeaseID && attempt.FencingToken == owner.FencingToken && attempt.RunID == owner.RunID && attempt.Status == "running", nil
}

func (c *conversationService) settleIdleConversationsPG(ctx context.Context, organization tracker.OrganizationID, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	before := now.Add(-c.config.SettleWindow)
	const cursor = "settle"
	after := c.lifecycleCursor(cursor)
	settled := 0
	var page conversationstore.ConversationPage
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleSettle, AfterID: after, Before: before, Limit: conversationSettleBatch})
		return err
	}); err != nil {
		return settled, fmt.Errorf("list PostgreSQL conversations to settle: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	var errs []error
	for _, candidate := range page.Items {
		changed, err := c.settleIdleConversationPG(ctx, organization, candidate, before, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("settle PostgreSQL conversation %s: %w", candidate.ID, err))
			continue
		}
		if changed {
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

func (c *conversationService) settleIdleConversationPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation, before, now time.Time) (bool, error) {
	changed := false
	err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		stored, err := writer.LockConversation(ctx, candidate.ProjectID, candidate.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		activity := stored.UpdatedAt
		if stored.LastMessageAt != nil {
			activity = *stored.LastMessageAt
		}
		finished := stored.Execution.Status == conversation.ExecutionIdle || stored.Execution.Status.Terminal()
		if stored.Status != conversation.StatusActive || !finished || !activity.Before(before) {
			return nil
		}
		stored.Status, stored.UpdatedAt = conversation.StatusSettled, now
		stored.SettledAt = &now
		updated, err := writer.UpdateConversation(ctx, stored, stored.Revision)
		if err != nil {
			return err
		}
		updatedRecord := conversationRecordFromStore(updated)
		if err := appendPGConversationEvent(ctx, writer, organization, updated.ID, conversation.EventConversationUpdated, projectConversation(updatedRecord), now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if changed {
		var record conversationRecord
		if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			record, err = readConversationRecordFromTenant(ctx, reader, candidate.ProjectID, candidate.ID)
			return err
		}); err != nil {
			return false, err
		}
		c.committed(record)
	}
	return changed, nil
}
