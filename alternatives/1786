package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	ConversationControlMaxWait     = 30 * time.Second
	ConversationMaxDeltaBytes      = 64 * 1024
	ConversationMaxSummaryBytes    = 4000
	ConversationMaxItemDataBytes   = 16 * 1024
	ConversationMaxTurnEvents      = 500
	ConversationHeaderLease        = "X-Detent-Lease"
	ConversationHeaderFencingToken = "X-Detent-Fencing-Token"
	ConversationHeaderAttempt      = "X-Detent-Attempt"
	ConversationQuestionPrompt     = "Waiting for your answer"
	ConversationLeaseLostError     = "runner lease lost"
)

// WorkerIdentity is the lease identity every worker request
// carries: in the JSON body, or in headers for GET.
type WorkerIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token"`
	AttemptID    string               `json:"attempt_id"`
}

type ConversationResumeResource struct {
	ThreadID string `json:"thread_id"`
	// ThreadOrigin names the kind of turn the thread the runner is about to
	// use belongs to: conversation.ThreadOriginCoordinator or
	// conversation.ThreadOriginWorker. It is the origin this bind's turn
	// records, so a runner holding a thread of the other kind in its own
	// registry can see that the hub did not hand it back.
	// Empty when the bind resumed nothing and started no turn.
	ThreadOrigin string `json:"thread_origin,omitempty"`
	// Transcript carries the recent history when the provider thread cannot
	// be resumed, so the runner can prepend it as clearly delimited data.
	Transcript []ConversationTranscriptEntry `json:"transcript,omitempty"`
}

func ConversationWorkerAuthorityUnavailable() error {
	return &hubapi.Error{Code: "unavailable", Message: "Worker identity authority is unavailable", Status: http.StatusServiceUnavailable}
}

// requireRecordOwner checks that the conversation's execution is owned by
// the caller's generation. Terminal executions are stale unless allowed.
func RequireConversationRecordOwner(record ConversationRecord, identity WorkerIdentity, allowTerminal bool) error {
	owner := record.Execution.Owner
	if owner.AttemptID != identity.AttemptID || owner.LeaseID != string(identity.LeaseID) || owner.FencingToken != int64(identity.FencingToken) {
		return ConversationStaleOwner("The conversation is not bound to this attempt", identity.AttemptID, owner.AttemptID)
	}
	if !allowTerminal && record.Execution.Status.Terminal() {
		return ConversationStale("The execution has ended")
	}
	return nil
}

type PgWorkerPublicationKey struct{}

func appendPreparedPGWorkerEvent(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, id string, kind conversation.EventType, body any, at time.Time) (bool, error) {
	plan, ok := ctx.Value(PgWorkerPublicationKey{}).(*ConversationPublicationPlan)
	if !ok {
		return false, nil
	}
	if message, ok := body.(ConversationMessageResource); ok {
		message.Seq = 0
		if kind == conversation.EventMessageUpdated {
			if cached, found := plan.Messages[message.ID]; found && cached.ContentID == message.contentID {
				message.Text, message.Data = cached.Text, ProjectMessage(cached).Data
			}
		}
		body = message
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return true, err
	}
	content, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return true, err
	}
	ref, ok := plan.Prepared[string(organization)+"/"+artifact.Digest(content)]
	if !ok {
		return false, nil
	}
	_, err = writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: id, ContentReference: ContentArchiveID(string(organization), ref), Event: conversation.Event{Type: kind, Data: raw, CreatedAt: at}})
	return true, err
}

func settlePGWorkerMessagesBatch(ctx context.Context, writer tenantstore.Writer, record *ConversationRecord, status conversation.ExecutionStatus, now time.Time, limit int) (int, bool, error) {
	type batch struct {
		filter   conversationstore.MessageFilter
		delivery conversation.Delivery
		failure  bool
	}
	owner := record.Execution.Owner
	batches := []batch{
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: owner.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}}, delivery: ConversationDeliveryForExecution(status)},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySending}}, delivery: conversation.DeliveryUnknown, failure: true},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySent}}, delivery: ConversationDeliveryForExecution(status)},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageAnswer, Deliveries: []conversation.Delivery{conversation.DeliveryQueued}}, delivery: conversation.DeliveryUnknown, failure: true},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageInterrupt, Deliveries: []conversation.Delivery{conversation.DeliveryQueued}}, delivery: conversation.DeliveryUnknown, failure: true},
	}
	processed := 0
	for _, current := range batches {
		remaining := limit - processed
		if remaining <= 0 {
			return processed, true, nil
		}
		current.filter.Limit = remaining
		page, err := writer.FilteredMessages(ctx, current.filter)
		if err != nil {
			return processed, false, err
		}
		for _, message := range page.Items {
			message.Delivery, message.UpdatedAt = current.delivery, now
			var failure *conversation.ReceiptError
			if current.failure {
				failure = &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control could be established"}
			}
			if err := UpdatePGWorkerMessage(ctx, writer, message, now, failure); err != nil {
				return processed, false, fmt.Errorf("settle conversation worker message: %w", err)
			}
			processed++
		}
		if page.Next != 0 || processed == limit {
			return processed, true, nil
		}
	}
	return processed, false, nil
}

// ConversationTranscriptEntry is one bounded history line for a runner that
// starts a fresh provider thread.
type ConversationTranscriptEntry struct {
	Role conversation.Role        `json:"role"`
	Kind conversation.MessageKind `json:"kind"`
	Text string                   `json:"text"`
}

func ConversationDeliveryForExecution(status conversation.ExecutionStatus) conversation.Delivery {
	switch status {
	case conversation.ExecutionCompleted:
		return conversation.DeliveryCompleted
	case conversation.ExecutionFailed:
		return conversation.DeliveryFailed
	case conversation.ExecutionInterrupted:
		return conversation.DeliveryInterrupted
	default:
		return conversation.DeliveryUnknown
	}
}

func UpdatePGWorkerMessage(ctx context.Context, writer tenantstore.Writer, message conversationstore.Message, now time.Time, failure *conversation.ReceiptError) error {
	updated, err := writer.UpdateMessage(ctx, message, message.Revision)
	if err != nil {
		return err
	}
	if err := AppendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventMessageUpdated, ProjectMessage(ConversationMessageFromStore(updated)), now); err != nil {
		return err
	}
	if message.CommandKey == "" {
		return nil
	}
	command, found, err := writer.Command(ctx, message.ConversationID, message.CommandKey)
	if err != nil || !found {
		return err
	}
	command.Receipt.Status, command.Receipt.Error, command.Receipt.UpdatedAt = message.Delivery, failure, now
	if command.Receipt.MessageID == "" {
		command.Receipt.MessageID = message.ID
	}
	if err := writer.UpdateCommand(ctx, command); err != nil {
		return err
	}
	return AppendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventCommandReceipt, command.Receipt, now)
}

func (identity WorkerIdentity) Validate() error {
	if strings.TrimSpace(string(identity.LeaseID)) == "" || identity.FencingToken <= 0 || strings.TrimSpace(identity.AttemptID) == "" {
		return hubapi.PublicInvalid("lease_id, fencing_token and attempt_id are required")
	}
	return nil
}

func (c *ConversationService) ReconcileExecutions(ctx context.Context) error {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return err
	}
	return c.ReconcileExecutionsPG(ctx, organization)
}

func (c *ConversationService) LeaseReleasedTenant(ctx context.Context, writer tenantstore.Writer, leaseID string, fencingToken int64, _ string) error {
	stored, err := writer.ConversationByExecutionLease(ctx, leaseID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	locked, err := writer.LockConversation(ctx, stored.ProjectID, stored.ID)
	if err != nil {
		return err
	}
	record := ConversationRecordFromStore(locked)
	if record.Execution.Owner.LeaseID != leaseID || record.Execution.Owner.FencingToken != fencingToken || record.Execution.Settled {
		return nil
	}
	lease, err := writer.Lease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.FencingToken != fencingToken || lease.ReleasedAt == nil {
		return nil
	}
	releasedAt := *lease.ReleasedAt
	if lease.RenewedAt.After(releasedAt) {
		return nil
	}
	now := c.Server.config().Now()
	if now.Before(releasedAt) {
		now = releasedAt
	}
	status := record.Execution.Status
	executionError := record.Execution.Error
	if !status.Terminal() {
		status = conversation.ExecutionInterrupted
		executionError = ConversationLeaseLostError
	}
	if err := SettlePGWorkerMessages(ctx, writer, &record, status, now, ConversationPGWorkerPageSize); err != nil {
		return fmt.Errorf("settle released conversation worker messages: %w", err)
	}
	record.Execution.Error = executionError
	if err := ExpirePGOutstandingQuestions(ctx, writer, record.ID, record.Execution.Owner.AttemptID, "", func(question conversationstore.Question) error {
		question.Status, question.UpdatedAt = conversation.QuestionExpired, now
		if err := writer.UpsertQuestion(ctx, question); err != nil {
			return err
		}
		return AppendPGConversationEvent(ctx, writer, record.OrganizationID, record.ID, conversation.EventQuestionUpdated, question.Question, now)
	}); err != nil {
		return err
	}
	current, err := writer.Conversation(ctx, record.ProjectID, record.ID)
	if err != nil {
		return err
	}
	current.Execution, current.UpdatedAt = record.Execution, now
	if current.Execution.Status == conversation.ExecutionCompleted {
		current.ExecutionContinuation = false
	}
	updated, err := writer.UpdateConversation(ctx, current, current.Revision)
	if err != nil {
		return err
	}
	return AppendPGConversationEvent(ctx, writer, record.OrganizationID, record.ID, conversation.EventExecutionUpdated, ProjectExecution(updated.Execution), now)
}

const ConversationPGWorkerPageSize = corestore.MaxPageSize

func ExpirePGOutstandingQuestions(ctx context.Context, writer conversationstore.Writer, conversationID, attemptID, turnID string, expire func(conversationstore.Question) error) error {
	after := ""
	for {
		page, err := writer.OutstandingQuestions(ctx, conversationID, attemptID, turnID, after, ConversationPGWorkerPageSize)
		if err != nil {
			return err
		}
		for _, question := range page.Items {
			if err := expire(question); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
}

func SettlePGWorkerMessages(ctx context.Context, writer tenantstore.Writer, record *ConversationRecord, status conversation.ExecutionStatus, now time.Time, limit int) error {
	before := int64(0)
	for {
		responding, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: record.Execution.Owner.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}, BeforeSequence: before, Limit: limit})
		if err != nil {
			return err
		}
		for _, message := range responding.Items {
			message.Delivery, message.UpdatedAt = ConversationDeliveryForExecution(status), now
			if err := UpdatePGWorkerMessage(ctx, writer, message, now, nil); err != nil {
				return fmt.Errorf("settle responding conversation message: %w", err)
			}
		}
		if responding.Next == 0 {
			break
		}
		before = responding.Items[0].Sequence
	}
	before = 0
	for {
		controls, err := writer.FilteredMessages(ctx, conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliveryQueued, conversation.DeliverySending, conversation.DeliverySent}, BeforeSequence: before, Limit: limit})
		if err != nil {
			return err
		}
		for _, message := range controls.Items {
			switch {
			case message.Delivery == conversation.DeliverySent:
				message.Delivery = ConversationDeliveryForExecution(status)
			case message.Kind == conversation.MessageAnswer || message.Kind == conversation.MessageInterrupt:
				message.Delivery = conversation.DeliveryUnknown
			case message.Delivery == conversation.DeliverySending:
				message.Delivery = conversation.DeliveryUnknown
			default:
				continue
			}
			message.UpdatedAt = now
			failure := (*conversation.ReceiptError)(nil)
			if message.Delivery == conversation.DeliveryUnknown {
				failure = &conversation.ReceiptError{Code: "unknown", Message: "The execution ended before the control could be established"}
			}
			if err := UpdatePGWorkerMessage(ctx, writer, message, now, failure); err != nil {
				return fmt.Errorf("settle conversation control message: %w", err)
			}
		}
		if controls.Next == 0 {
			break
		}
		before = controls.Items[0].Sequence
	}
	record.Execution.Status, record.Execution.Error, record.Execution.Settled = status, "", true
	record.Execution.Capabilities = conversation.Capabilities{}
	record.Execution.UpdatedAt = now
	return nil
}
