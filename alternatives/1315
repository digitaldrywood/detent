package nativechange

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/readtools"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (e *nativeExecution) AgentTools() ([]runner.AgentTool, runner.AgentToolHandler) {
	var reader readtools.Reader
	if e.claim.source != nil {
		reader = e.claim.source.NativeClient()
	}
	return readtools.Tools(reader, e, func() time.Time { return e.scheduler.now() }, evidenceTool(), e.attachEvidence)
}
