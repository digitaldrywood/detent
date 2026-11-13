package conversations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ConversationLifecycleBatchSize = 200

func (c *ConversationService) LifecycleCursor(kind string) string {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return c.lifecycleCursors[kind]
}

func (c *ConversationService) advanceLifecycleCursor(kind, next string) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.lifecycleCursors == nil {
		c.lifecycleCursors = make(map[string]string)
	}
	c.lifecycleCursors[kind] = next
}

func (c *ConversationService) LifecycleOrganization() (tracker.OrganizationID, error) {
	if !c.Server.conversationRecordsSelected() {
		return "", errors.New("conversation tenant repository is required for PostgreSQL lifecycle work")
	}
	organization := strings.TrimSpace(c.Server.appOrganizationID())
	if organization == "" {
		return "", errors.New("conversation service organization is required for PostgreSQL lifecycle work")
	}
	return tracker.OrganizationID(organization), nil
}

func (c *ConversationService) normalizeAfterRestartPG(ctx context.Context, organization tracker.OrganizationID, now time.Time) (conversationNormalizeSummary, error) {
	var summary conversationNormalizeSummary
	after := ""
	for {
		var page conversationstore.ConversationPage
		if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleRestart, AfterID: after, Limit: ConversationLifecycleBatchSize})
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

func (c *ConversationService) normalizeRestartConversationBatchPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation, now time.Time) (conversationNormalizeSummary, bool, error) {
	var summary conversationNormalizeSummary
	more := false
	err := c.Server.config().TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
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
			Limit:          ConversationLifecycleBatchSize,
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
		questions, err := writer.RestartQuestions(ctx, stored.ID, afterQuestion, ConversationLifecycleBatchSize)
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

		commands, err := writer.PendingCommands(ctx, stored.ID, "", ConversationLifecycleBatchSize)
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

func (c *ConversationService) WakePendingPG(ctx context.Context, organization tracker.OrganizationID) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	const cursor = "pending"
	after := c.LifecycleCursor(cursor)
	coordinator, controls := 0, 0
	var page conversationstore.PendingConversationPage
	var errs []error
	if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.PendingConversations(ctx, after, ConversationLifecycleBatchSize)
		return err
	}); err != nil {
		return fmt.Errorf("list PostgreSQL conversations with pending messages: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	for _, pending := range page.Items {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if pending.Linked {
			c.Controls.Wake(pending.ID)
			controls++
			continue
		}
		if c.Coordinator.Available() {
			if err := c.wakeCoordinator(ctx, pending.ID); err != nil {
				if ctx.Err() != nil {
					return errors.Join(append(errs, ctx.Err())...)
				}
				errs = append(errs, fmt.Errorf("wake PostgreSQL coordinator %s: %w", pending.ID, err))
				continue
			}
			coordinator++
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(errs, err)...)
	}
	c.Logger.Info("conversation.wake_pending", "coordinator", coordinator, "controls", controls)
	return errors.Join(errs...)
}

func (c *ConversationService) ReconcileExecutionsPG(ctx context.Context, organization tracker.OrganizationID) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	const cursor = "execution"
	after := c.LifecycleCursor(cursor)
	var errs []error
	var page conversationstore.ConversationPage
	if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleExecution, AfterID: after, Limit: ConversationLifecycleBatchSize})
		return err
	}); err != nil {
		return fmt.Errorf("list PostgreSQL conversation executions: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	for _, candidate := range page.Items {
		changed, err := c.ReconcileExecutionPG(ctx, organization, candidate)
		if err != nil {
			errs = append(errs, fmt.Errorf("conversation %s: %w", candidate.ID, err))
			continue
		}
		if changed {
			var record ConversationRecord
			if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
				var err error
				record, err = ReadConversationRecordFromTenant(ctx, reader, candidate.ProjectID, candidate.ID)
				return err
			}); err != nil {
				errs = append(errs, fmt.Errorf("read reconciled PostgreSQL conversation %s: %w", candidate.ID, err))
				continue
			}
			c.Committed(record)
		}
	}
	return errors.Join(errs...)
}

func (c *ConversationService) ReconcileExecutionPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation) (bool, error) {
	if candidate.WorkItemID == "" || candidate.Execution.Owner.LeaseID == "" || candidate.Execution.Owner.AttemptID == "" {
		return false, nil
	}
	changed := false
	err := c.Server.config().TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		locker, ok := writer.(leasestore.FencedLeaseLocker)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		runner, runnerErr := writer.RunnerForLease(ctx, candidate.Execution.Owner.LeaseID)
		if runnerErr != nil && !errors.Is(runnerErr, corestore.ErrNotFound) {
			return runnerErr
		}
		if runnerErr == nil {
			if _, err := writer.AccessCredential(ctx, runner.TokenID); err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if _, err := writer.LockWorkItem(ctx, candidate.ProjectID, tracker.NativeWorkItemID(candidate.WorkItemID)); err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := locker.LockLease(ctx, candidate.Execution.Owner.LeaseID); err != nil && !errors.Is(err, tracker.ErrLeaseNotFound) && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		stored, err := writer.LockConversation(ctx, candidate.ProjectID, candidate.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if stored.WorkItemID != candidate.WorkItemID || stored.Execution.Owner != candidate.Execution.Owner {
			return nil
		}
		record := ConversationRecordFromStore(stored)
		if stored.WorkItemID == "" || record.Execution.Owner.LeaseID == "" || record.Execution.Owner.AttemptID == "" || (record.Execution.Status.Terminal() && record.Execution.Settled) {
			return nil
		}
		now := c.Server.config().Now()
		current, err := c.Server.ConversationLeaseCurrentPG(ctx, writer, record, now)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
		status := record.Execution.Status
		if !status.Terminal() {
			status = conversation.ExecutionInterrupted
			record.Execution.Error = ConversationLeaseLostError
		}
		processed, moreMessages, err := settlePGWorkerMessagesBatch(ctx, writer, &record, status, now, ConversationLifecycleBatchSize)
		if err != nil {
			return err
		}
		moreQuestions := false
		remaining := ConversationLifecycleBatchSize - processed
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
				if err := AppendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventQuestionUpdated, question.Question, now); err != nil {
					return err
				}
			}
			moreQuestions = questions.Next != ""
		}
		record.Execution.Status = status
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
		updatedRecord := ConversationRecordFromStore(updated)
		updatedRecord.WorkItem = record.WorkItem
		if err := AppendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventConversationUpdated, ProjectConversation(updatedRecord), now); err != nil {
			return err
		}
		if err := AppendPGConversationEvent(ctx, writer, organization, record.ID, conversation.EventExecutionUpdated, ProjectExecution(updated.Execution), now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *Server) ConversationLeaseCurrentPG(ctx context.Context, reader tenantstore.Reader, record ConversationRecord, now time.Time) (bool, error) {
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
	owner := record.Execution.Owner
	if lease.OrganizationID != record.OrganizationID || lease.IssueID != item.ID || lease.FencingToken != owner.FencingToken || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.RenewedAt.After(now) {
		return false, nil
	}
	runner, err := reader.RunnerForLease(ctx, lease.LeaseID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if runner.RunnerID != owner.RunnerID || string(runner.MachineID) != owner.MachineID || string(runner.MachineID) != lease.MachineID {
		return false, nil
	}
	credential, err := reader.AccessCredential(ctx, runner.TokenID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	identity := runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runner.RunnerID, MachineID: runner.MachineID}, OrganizationID: runner.OrganizationID, Scope: runner.Scope, Operations: runner.Operations, ExpiresAt: runner.ExpiresAt}
	authority := s.deps.WorkerAuthorityAt(now)
	if err := authority.VerifyConversationWorker(ctx, reader, ConversationWorkerIdentity{Credential: credential, Runner: identity, Organization: record.OrganizationID, Project: record.ProjectID}); err != nil {
		var refusal *hubapi.Error
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, corestore.ErrNotFound) || errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	if err := s.deps.CheckMonthlyLease(ctx, reader, record.ProjectID, lease.LeaseID, now); err != nil {
		if errors.Is(err, tracker.ErrLeaseConflict) {
			return false, nil
		}
		return false, err
	}
	attempt, err := reader.Attempt(ctx, owner.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		if record.Execution.Status != conversation.ExecutionStarting || record.Execution.Settled || !hubapi.ValidID(owner.AttemptID, "attempt") || !hubapi.ValidID(owner.RunID, "run") {
			return false, nil
		}
		latest, latestErr := reader.LatestAttempt(ctx, item.ID)
		if errors.Is(latestErr, corestore.ErrNotFound) || errors.Is(latestErr, sql.ErrNoRows) {
			return true, nil
		}
		if latestErr != nil {
			return false, latestErr
		}
		if latest.LeaseID != lease.LeaseID {
			return true, nil
		}
		attempt, err = latest, nil
	}
	if err != nil {
		return false, err
	}
	return attempt.ID == owner.AttemptID && attempt.OrganizationID == record.OrganizationID && attempt.IssueID == item.ID && attempt.LeaseID == lease.LeaseID && attempt.FencingToken == owner.FencingToken && attempt.RunID == owner.RunID && attempt.Status == "running", nil
}

func (c *ConversationService) SettleIdleConversationsPG(ctx context.Context, organization tracker.OrganizationID, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	before := now.Add(-c.Config.SettleWindow)
	const cursor = "settle"
	after := c.LifecycleCursor(cursor)
	settled := 0
	var page conversationstore.ConversationPage
	if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		page, err = reader.LifecycleConversations(ctx, conversationstore.LifecycleQuery{Kind: conversationstore.LifecycleSettle, AfterID: after, Before: before, Limit: conversationSettleBatch})
		return err
	}); err != nil {
		return settled, fmt.Errorf("list PostgreSQL conversations to settle: %w", err)
	}
	c.advanceLifecycleCursor(cursor, page.Next)
	var errs []error
	for _, candidate := range page.Items {
		changed, err := c.SettleIdleConversationPG(ctx, organization, candidate, before, now)
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

func (c *ConversationService) SettleIdleConversationPG(ctx context.Context, organization tracker.OrganizationID, candidate conversationstore.Conversation, before, now time.Time) (bool, error) {
	changed := false
	err := c.Server.config().TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
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
		updatedRecord := ConversationRecordFromStore(updated)
		if err := AppendPGConversationEvent(ctx, writer, organization, updated.ID, conversation.EventConversationUpdated, ProjectConversation(updatedRecord), now); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if changed {
		var record ConversationRecord
		if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			record, err = ReadConversationRecordFromTenant(ctx, reader, candidate.ProjectID, candidate.ID)
			return err
		}); err != nil {
			return false, err
		}
		c.Committed(record)
	}
	return changed, nil
}
