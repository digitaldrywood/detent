package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) commandCreateAction(ctx context.Context, scope hubscope.NativeScope, request projectActionRequest) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "POST", "", request.Mutation, request)
}
func (s *Service) commandPatchAction(ctx context.Context, scope hubscope.NativeScope, id string, request projectActionPatch) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "PATCH", id, request.Mutation, request)
}
func (s *Service) commandRunAction(ctx context.Context, scope hubscope.NativeScope, id string, request projectActionRunRequest) (json.RawMessage, error) {
	return s.queueActionRunRecord(ctx, scope, id, request)
}
func (s *Service) commandDeleteAction(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	return s.mutateActionRecord(ctx, scope, "DELETE", id, tracker.Mutation{}, nil)
}

func (s *Service) readAttachment(ctx context.Context, scope hubscope.NativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	return s.readAttachmentPG(ctx, scope, conversationID, id)
}

// runner grants are required for headless execution too. A token retains the
// dashboard's existing non-person restrictions on interactive terminals.
