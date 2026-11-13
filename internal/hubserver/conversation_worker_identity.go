package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	conversationControlMaxWait     = 30 * time.Second
	conversationMaxDeltaBytes      = 64 * 1024
	conversationMaxSummaryBytes    = 4000
	conversationMaxItemDataBytes   = 16 * 1024
	conversationMaxTurnEvents      = 500
	conversationHeaderLease        = "X-Detent-Lease"
	conversationHeaderFencingToken = "X-Detent-Fencing-Token"
	conversationHeaderAttempt      = "X-Detent-Attempt"
	conversationQuestionPrompt     = "Waiting for your answer"
	conversationLeaseLostError     = "runner lease lost"
)

// conversationWorkerIdentity is the lease identity every worker request
// carries: in the JSON body, or in headers for GET.
type conversationWorkerIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token"`
	AttemptID    string               `json:"attempt_id"`
}

type conversationResumeResource struct {
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
	Transcript []conversationTranscriptEntry `json:"transcript,omitempty"`
}

func conversationWorkerAuthorityUnavailable() error {
	return &nativeError{Code: "unavailable", Message: "Worker identity authority is unavailable", status: http.StatusServiceUnavailable}
}

// requireRecordOwner checks that the conversation's execution is owned by
// the caller's generation. Terminal executions are stale unless allowed.
func requireConversationRecordOwner(record conversationRecord, identity conversationWorkerIdentity, allowTerminal bool) error {
	owner := record.Execution.Owner
	if owner.AttemptID != identity.AttemptID || owner.LeaseID != string(identity.LeaseID) || owner.FencingToken != int64(identity.FencingToken) {
		return conversationStaleOwner("The conversation is not bound to this attempt", identity.AttemptID, owner.AttemptID)
	}
	if !allowTerminal && record.Execution.Status.Terminal() {
		return conversationStale("The execution has ended")
	}
	return nil
}

type pgWorkerPublicationKey struct{}

func appendPreparedPGWorkerEvent(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, id string, kind conversation.EventType, body any, at time.Time) (bool, error) {
	plan, ok := ctx.Value(pgWorkerPublicationKey{}).(*conversationPublicationPlan)
	if !ok {
		return false, nil
	}
	if message, ok := body.(conversationMessageResource); ok {
		message.Seq = 0
		if kind == conversation.EventMessageUpdated {
			if cached, found := plan.messages[message.ID]; found && cached.ContentID == message.contentID {
				message.Text, message.Data = cached.Text, projectMessage(cached).Data
			}
		}
		body = message
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return true, err
	}
	content, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return true, err
	}
	ref, ok := plan.prepared[string(organization)+"/"+artifact.Digest(content)]
	if !ok {
		return false, nil
	}
	_, err = writer.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: id, ContentReference: contentArchiveID(string(organization), ref), Event: conversation.Event{Type: kind, Data: raw, CreatedAt: at}})
	return true, err
}

func settlePGWorkerMessagesBatch(ctx context.Context, writer tenantstore.Writer, record *conversationRecord, status conversation.ExecutionStatus, now time.Time, limit int) (int, bool, error) {
	type batch struct {
		filter   conversationstore.MessageFilter
		delivery conversation.Delivery
		failure  bool
	}
	owner := record.Execution.Owner
	batches := []batch{
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, AttemptID: owner.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}}, delivery: conversationDeliveryForExecution(status)},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySending}}, delivery: conversation.DeliveryUnknown, failure: true},
		{filter: conversationstore.MessageFilter{ConversationID: record.ID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySent}}, delivery: conversationDeliveryForExecution(status)},
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
			if err := updatePGWorkerMessage(ctx, writer, message, now, failure); err != nil {
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

// conversationTranscriptEntry is one bounded history line for a runner that
// starts a fresh provider thread.
type conversationTranscriptEntry struct {
	Role conversation.Role        `json:"role"`
	Kind conversation.MessageKind `json:"kind"`
	Text string                   `json:"text"`
}

func conversationDeliveryForExecution(status conversation.ExecutionStatus) conversation.Delivery {
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

func updatePGWorkerMessage(ctx context.Context, writer tenantstore.Writer, message conversationstore.Message, now time.Time, failure *conversation.ReceiptError) error {
	updated, err := writer.UpdateMessage(ctx, message, message.Revision)
	if err != nil {
		return err
	}
	if err := appendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventMessageUpdated, projectMessage(conversationMessageFromStore(updated)), now); err != nil {
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
	return appendPGConversationEvent(ctx, writer, message.OrganizationID, message.ConversationID, conversation.EventCommandReceipt, command.Receipt, now)
}
