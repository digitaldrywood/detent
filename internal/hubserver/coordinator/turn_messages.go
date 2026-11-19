package coordinator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	PendingMessages    = 20
	transcriptMessages = 20
)

func (d RecordDeps) PendingMessages(ctx context.Context, id string) ([]conversations.ConversationMessageRecord, error) {
	return d.ReadMessages(ctx, conversationstore.MessageFilter{ConversationID: id, Role: conversation.RoleUser, Kind: conversation.MessageText, Deliveries: []conversation.Delivery{conversation.DeliverySaved, conversation.DeliveryQueued}, Limit: PendingMessages, Oldest: true})
}

func (d RecordDeps) transcript(ctx context.Context, conversationID string, beforeSeq int64) ([]conversations.ConversationMessageRecord, error) {
	messages, err := d.ReadMessages(ctx, conversationstore.MessageFilter{ConversationID: conversationID, BeforeSequence: beforeSeq, Limit: transcriptMessages})
	if err != nil {
		return nil, err
	}
	filtered := messages[:0]
	for _, message := range messages {
		if message.Kind == conversation.MessageText && (message.Role == conversation.RoleUser || message.Role == conversation.RoleAssistant) && strings.TrimSpace(message.Text) != "" {
			filtered = append(filtered, message)
		}
	}
	return filtered, nil
}

type TurnStartDeps struct {
	Write     TurnWriter
	Records   RecordDeps
	Admission TurnAdmissionDeps
}

type StartedTurn struct {
	ModelChoice    bool
	Users          []conversations.ConversationMessageRecord
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ThreadID       string
	Preferences    conversation.Preferences
	Assistant      conversations.ConversationMessageRecord
	Transcript     []conversations.ConversationMessageRecord
	Refusal        error
}

func (d TurnStartDeps) Start(ctx context.Context, conversationID string, turnID func(string)) (StartedTurn, error) {
	var started StartedTurn
	var pending []conversations.ConversationMessageRecord
	err := d.Write(ctx, conversationID, func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
		if !Handles(*record) {
			return ErrNothingPending
		}
		if len(pending) == 0 {
			return ErrNothingPending
		}
		var err error
		started.ModelChoice, started.Refusal, err = d.Admission.Check(ctx, tx, *record, pending[len(pending)-1].Actor.PrincipalID, now, started.ModelChoice)
		if err != nil {
			return err
		}
		for i := range pending {
			pending[i].Delivery = conversation.DeliverySending
			if err := UpdateMessage(ctx, tx, pending[i], now); err != nil {
				return err
			}
			pending[i].UpdatedAt = now
		}
		started.Users = pending
		started.OrganizationID = record.OrganizationID
		started.ProjectID = record.ProjectID
		started.ThreadID = record.ProviderThreadID
		started.Preferences = record.Preferences
		if err := AppendMessage(ctx, tx, record, &started.Assistant, now); err != nil {
			return err
		}
		execution := conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{ThreadID: record.ProviderThreadID, TurnID: started.Assistant.ID}}
		return UpdateExecution(ctx, tx, record, execution, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		var err error
		pending, err = d.Records.PendingMessages(p.Context(ctx), p.Record.ID)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return ErrNothingPending
		}
		if p.Record.ProviderThreadID == "" {
			started.Transcript, err = d.Records.transcript(p.Context(ctx), p.Record.ID, pending[0].Seq)
			if err != nil {
				return err
			}
		}
		for _, message := range pending {
			message.Delivery = conversation.DeliverySending
			if err := p.UpdateMessage(ctx, message); err != nil {
				return err
			}
		}
		if started.Assistant.ID == "" {
			started.Assistant = conversations.ConversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
		}
		started.Assistant.ThreadID = p.Record.ProviderThreadID
		if err := p.AppendMessage(ctx, &started.Assistant); err != nil {
			return err
		}
		turnID(started.Assistant.ID)
		return p.UpdateExecution(ctx, conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{ThreadID: p.Record.ProviderThreadID, TurnID: started.Assistant.ID}})
	})
	return started, err
}

func PublishProviderThread(ctx context.Context, write TurnWriter, id, threadID string) error {
	return write(ctx, id, func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error {
		if record.ProviderThreadID == threadID && record.ProviderThreadRunnerID == "" && record.ProviderThreadOrigin == conversation.ThreadOriginCoordinator {
			return nil
		}
		record.ProviderThreadID = threadID
		record.ProviderThreadRunnerID = ""
		record.ProviderThreadOrigin = conversation.ThreadOriginCoordinator
		return SaveConversation(ctx, tx, record, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		if p.Record.ProviderThreadID == threadID && p.Record.ProviderThreadRunnerID == "" && p.Record.ProviderThreadOrigin == conversation.ThreadOriginCoordinator {
			return nil
		}
		p.Record.ProviderThreadID = threadID
		p.Record.ProviderThreadRunnerID = ""
		p.Record.ProviderThreadOrigin = conversation.ThreadOriginCoordinator
		return p.SaveConversation(ctx)
	})
}
