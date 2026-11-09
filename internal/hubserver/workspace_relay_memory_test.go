package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceRelayClosedStreamCannotChangeMemory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*workspaceRelay, *relayStream)
	}{
		{name: "late output", mutate: func(relay *workspaceRelay, stream *relayStream) {
			relay.bufferForPerson("ws_example", stream, workspacesession.Frame{Payload: []byte("late output")}, 2)
		}},
		{name: "late acknowledgement", mutate: func(relay *workspaceRelay, stream *relayStream) {
			relay.acknowledge("ws_example", stream, 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			const frameBytes int64 = 100
			relay := newWorkspaceRelay(&workspaceService{config: WorkspaceConfig{RelayMemoryBytes: 1 << 20}})
			closed := &relayStream{id: "relayconn_closed:1", bufferBytes: frameBytes, buffer: []relayBufferedFrame{{seq: 1, bytes: frameBytes}}}
			live := &relayStream{id: "relayconn_live:1", bufferBytes: frameBytes, buffer: []relayBufferedFrame{{seq: 1, bytes: frameBytes}}}
			relay.rooms["ws_example"] = &relayRoom{
				workspaceID: "ws_example",
				people:      map[string]*relayConnection{},
				streams:     map[string]*relayStream{closed.id: closed, live.id: live},
				detached:    map[string]*relayStream{},
			}
			relay.memory = 2 * frameBytes
			relay.closeStream(t.Context(), "ws_example", closed.id)
			test.mutate(relay, closed)
			if relay.memory != frameBytes || live.bufferBytes != frameBytes || len(live.buffer) != 1 {
				t.Fatalf("closed stream changed surviving memory: relay=%d stream=%d frames=%d", relay.memory, live.bufferBytes, len(live.buffer))
			}
		})
	}
}
