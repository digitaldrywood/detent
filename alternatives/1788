package conversations

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type ConversationPublicationKey struct{}

func (p *ConversationPublicationPlan) Context(ctx context.Context) context.Context {
	if p.Prepared == nil {
		p.Prepared = PreparedConversationContents{}
		if existing, ok := ctx.Value(ConversationContentsKey{}).(PreparedConversationContents); ok {
			for key, ref := range existing {
				p.Prepared[key] = ref
			}
		}
	}
	if p.Reads == nil {
		p.Reads = ConversationArchiveCache(ctx)
		if p.Reads == nil {
			p.Reads = &ConversationArchiveReads{Content: map[string][]byte{}}
		}
	}
	ctx = context.WithValue(ctx, ConversationContentsKey{}, p.Prepared)
	ctx = context.WithValue(ctx, conversationArchiveReadsKey{}, p.Reads)
	if p.retryMessage != nil {
		ctx = context.WithValue(ctx, conversationPreparedRetryKey{}, *p.retryMessage)
	}
	return context.WithValue(ctx, ConversationPublicationKey{}, p)
}

func ConversationQuestionID(ctx context.Context) string {
	if p, ok := ctx.Value(ConversationPublicationKey{}).(*ConversationPublicationPlan); ok && p.questionCursor < len(p.questionIDs) {
		id := p.questionIDs[p.questionCursor]
		p.questionCursor++
		return id
	}
	return conversation.NewQuestionID()
}

func conversationReadProjection(ctx context.Context, query Queryer, id string) string {
	if _, transactional := query.(*sql.Tx); transactional {
		if plan, ok := ctx.Value(ConversationPublicationKey{}).(*ConversationPublicationPlan); ok && plan.Record.ID == id {
			return conversationColumns + "," + strconv.FormatInt(plan.Original.MessageCount+plan.insertedMessages, 10) + ",read_audience"
		}
	}
	return conversationReadColumns
}
