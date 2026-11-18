package scenariotest

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

// nativeSSHExecution uses the same bootstrap serialization and bidirectional
// callbacks as a remote worker, with no access to central filesystem paths.
func nativeSSHExecution(t *testing.T, ctx context.Context, execution runner.Execution, journalRoot string) (runner.Execution, func()) {
	t.Helper()
	left, right := net.Pipe()
	request := runner.RunRequest{Execution: execution}
	callbacks := (&runner.Runner{}).SSHRunCallbacks(request)
	central := runner.NewSSHPeer(ctx, left, left, callbacks.Handle)
	sources := &runner.SSHExecutionSources{}
	remote := runner.NewSSHPeer(ctx, right, right, sources.Handle)
	callbacks.BindExecutionSources(central, journalRoot)
	closePeers := func() {
		central.Close()
		remote.Close()
		left.Close()
		right.Close()
	}
	t.Cleanup(closePeers)
	data, err := json.Marshal(runner.NewSSHRunRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	var wire runner.SSHRunRequest
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wire.Sources = sources
	return wire.Bind(ctx, remote).Execution, closePeers
}
