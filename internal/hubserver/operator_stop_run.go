package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e nativeOperatorExecutor) stopRunFeature() operatorcommands.StopRunDeps {
	s := e.service
	return operatorcommands.StopRunDeps{
		ResolveItem: func(ctx context.Context, scope hubscope.NativeScope, reference string) (tracker.NativeIssue, error) {
			return s.resolveOperatorNativeItem(ctx, nil, scope, reference)
		},
		SafeError: hubSafeChangeError,
		Audit:     s.hubChangeAudit,
		Submit: func(ctx context.Context, action chat.Action) (chat.Action, error) {
			return s.operatorChat.Submit(ctx, action)
		},
		Result: s.hubActionResult,
		Stop:   s.stopNativeRunCommand,
	}
}

func (e nativeOperatorExecutor) stopRunAuthority(ctx context.Context, raw json.RawMessage) (context.Context, hubscope.NativeScope, operatortool.NativeStopRunArguments, error) {
	return e.stopRunFeature().Authorize(ctx, raw)
}

func (e nativeOperatorExecutor) stopRun(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	return e.stopRunFeature().SubmitCall(ctx, call)
}

func (e nativeOperatorExecutor) executeStopRun(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return e.stopRunFeature().Execute(ctx, action)
}
