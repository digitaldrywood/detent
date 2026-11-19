package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func (s *Service) readWorkspaceFilesTool(ctx context.Context, scope hubscope.NativeScope, name string, request workspaceToolRequest) (json.RawMessage, error) {
	return s.relayHTTP(s.workspaces).ReadOperatorFiles(ctx, scope, name, request)
}
