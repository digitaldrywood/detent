package conversations

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
)

func SetArchiveWriting(ctx context.Context, writing bool) {
	if content, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok {
		content.Mu.Lock()
		content.Writing = writing
		if writing {
			clear(content.IDPositions)
		}
		content.Mu.Unlock()
	}
}
