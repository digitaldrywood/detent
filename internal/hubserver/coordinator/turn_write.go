package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

type TurnWriteDeps struct {
	Service     *conversations.ConversationService
	Now         func() time.Time
	WriteTenant func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
}

// write runs fn against a fresh copy of the conversation inside one
// transaction, commits and reports the commit to the service. Reading the
// record inside the transaction keeps revision checks exact even when the
// API changed the conversation during the turn.
func (c TurnWriteDeps) Write(ctx context.Context, conversationID string, fn func(ctx context.Context, tx tenantstore.Writer, record *conversations.ConversationRecord, now time.Time) error, preparations ...func(context.Context, *conversations.ConversationPublicationPlan) error) error {
	now := c.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		prepared := conversations.NewConversationArchiveReads(ctx)
		original, err := c.Service.ReadPublicationConversation(prepared, conversationID)
		if err != nil {
			return err
		}
		if !Handles(original) {
			return ErrLinked
		}
		plan := conversations.ConversationPublicationPlan{Service: c.Service, Record: original, Original: original, Now: now, Coordinator: true}
		for _, prepare := range preparations {
			if err := prepare(prepared, &plan); err != nil {
				return err
			}
		}
		for id, message := range plan.Messages {
			message, err = plan.Message(prepared, message)
			if err != nil {
				return err
			}
			plan.Messages[id] = message
		}
		for _, body := range plan.Events {
			raw, err := json.Marshal(body)
			if err != nil {
				return err
			}
			if err := plan.PrepareValue(prepared, raw); err != nil {
				return err
			}
		}
		var committed conversations.ConversationRecord
		err = c.WriteTenant(plan.Context(prepared), original.OrganizationID, func(w tenantstore.Writer) error {
			ctx := plan.Context(prepared)
			stored, err := w.LockConversation(ctx, original.ProjectID, original.ID)
			if err != nil {
				return err
			}
			if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
				return ErrRevision
			}
			record := conversations.ConversationRecordFromStore(stored)
			if !Handles(record) {
				return ErrLinked
			}
			if err := conversations.RegisterPreparedConversationObjects(ctx, w, original.OrganizationID, plan.Prepared, now); err != nil {
				return err
			}
			if err := fn(ctx, w, &record, now); err != nil {
				return err
			}
			stored, err = w.ConversationByID(ctx, record.ID)
			committed = conversations.ConversationRecordFromStore(stored)
			return err
		})
		if errors.Is(err, ErrRevision) {
			continue
		}
		if err == nil {
			c.Service.Committed(committed)
		}
		return err
	}
}
