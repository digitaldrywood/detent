package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationPublicationKey struct{}

func (c *conversationService) newPublicationPlan(ctx context.Context, id string) (*conversationPublicationPlan, error) {
	ctx = newConversationArchiveReads(ctx)
	record, err := c.store.readConversationByID(ctx, c.store.reader, id)
	if err != nil {
		return nil, err
	}
	if err := resolveConversationWorkItems(ctx, c.store.reader, []*conversationRecord{&record}); err != nil {
		return nil, err
	}
	now, err := c.server.database.currentTime()
	if err != nil {
		return nil, err
	}
	return &conversationPublicationPlan{service: c, record: record, original: record, now: now, reads: conversationArchiveCache(ctx)}, nil
}

func (p *conversationPublicationPlan) context(ctx context.Context) context.Context {
	if p.prepared == nil {
		p.prepared = preparedConversationContents{}
		if existing, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents); ok {
			for key, ref := range existing {
				p.prepared[key] = ref
			}
		}
	}
	if p.reads == nil {
		p.reads = conversationArchiveCache(ctx)
		if p.reads == nil {
			p.reads = &conversationArchiveReads{content: map[string][]byte{}}
		}
	}
	if p.toolRevisions != nil {
		ctx = context.WithValue(ctx, legacyToolRevisionKey{}, p.toolRevisions)
	}
	ctx = context.WithValue(ctx, conversationContentsKey{}, p.prepared)
	ctx = context.WithValue(ctx, conversationArchiveReadsKey{}, p.reads)
	if p.retryMessage != nil {
		ctx = context.WithValue(ctx, conversationPreparedRetryKey{}, *p.retryMessage)
	}
	if p.issueAllocation != nil {
		ctx = context.WithValue(ctx, conversationIssueAllocationKey{}, *p.issueAllocation)
	}
	return context.WithValue(ctx, conversationPublicationKey{}, p)
}

func (p *conversationPublicationPlan) validate(ctx context.Context, tx *sql.Tx) error {
	if p.original.ID == "" {
		return nil
	}
	var revision, sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT revision,event_seq FROM conversations WHERE id=?", p.original.ID).Scan(&revision, &sequence); err != nil {
		return err
	}
	if revision != p.original.Revision || sequence != p.original.EventSeq {
		return conversationStale("The conversation changed during content preparation")
	}
	return nil
}

func conversationWriteTime(ctx context.Context, now time.Time) time.Time {
	if p, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok {
		return p.now
	}
	return now
}

func validateConversationPublication(ctx context.Context, tx *sql.Tx) error {
	if p, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok {
		return p.validate(ctx, tx)
	}
	return nil
}

func rollbackConversationPreparation(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}

func conversationQuestionID(ctx context.Context) string {
	if p, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && p.questionCursor < len(p.questionIDs) {
		id := p.questionIDs[p.questionCursor]
		p.questionCursor++
		return id
	}
	return conversation.NewQuestionID()
}

func conversationReadProjection(ctx context.Context, query nativeQueryer, id string) string {
	if _, transactional := query.(*sql.Tx); transactional {
		if plan, ok := ctx.Value(conversationPublicationKey{}).(*conversationPublicationPlan); ok && plan.record.ID == id {
			return conversationColumns + "," + strconv.FormatInt(plan.original.MessageCount+plan.insertedMessages, 10) + ",read_audience"
		}
	}
	return conversationReadColumns
}
