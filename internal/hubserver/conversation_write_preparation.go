package hubserver

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationPublicationKey struct{}

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
	ctx = context.WithValue(ctx, conversationContentsKey{}, p.prepared)
	ctx = context.WithValue(ctx, conversationArchiveReadsKey{}, p.reads)
	if p.retryMessage != nil {
		ctx = context.WithValue(ctx, conversationPreparedRetryKey{}, *p.retryMessage)
	}
	return context.WithValue(ctx, conversationPublicationKey{}, p)
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
