package workspacerelay

import (
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceRelayRunnerStreamFencesSupersededRunners(t *testing.T) {
	t.Parallel()
	current := &Connection{ID: "relayconn_current", WorkspaceID: "ws_1", Runner: true}
	superseded := &Connection{ID: "relayconn_old", WorkspaceID: "ws_1", Runner: true}
	elsewhere := &Connection{ID: "relayconn_other", WorkspaceID: "ws_2", Runner: true}
	person := &Connection{ID: "relayconn_person", WorkspaceID: "ws_1"}
	live := &Stream{ID: "relayconn_person:1", Channel: workspacesession.ChannelFiles, ConnectionID: person.ID}
	parked := &Stream{ID: "relayconn_person:2", Channel: workspacesession.ChannelFiles, ConnectionID: person.ID}
	relay := New(Deps{})
	relay.rooms["ws_1"] = &Room{
		WorkspaceID: "ws_1",
		Runner:      current,
		people:      map[string]*Connection{person.ID: person},
		Streams:     map[string]*Stream{live.ID: live},
		detached:    map[string]*Stream{parked.ID: parked},
		terminals:   map[string]*TerminalStream{},
	}
	tests := []struct {
		name       string
		runner     *Connection
		stream     string
		wantStream *Stream
		wantPerson *Connection
	}{
		{name: "current runner reaches a live stream", runner: current, stream: live.ID, wantStream: live, wantPerson: person},
		{name: "current runner reaches a detached stream", runner: current, stream: parked.ID, wantStream: parked},
		{name: "current runner naming no stream", runner: current, stream: "relayconn_person:9"},
		{name: "superseded runner is refused a live stream", runner: superseded, stream: live.ID},
		{name: "superseded runner is refused a detached stream", runner: superseded, stream: parked.ID},
		{name: "runner of another workspace", runner: elsewhere, stream: live.ID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stream, owner := relay.RunnerStream(test.runner, test.stream)
			if stream != test.wantStream || owner != test.wantPerson {
				t.Fatalf("runnerStream = (%v, %v), want (%v, %v)", stream, owner, test.wantStream, test.wantPerson)
			}
		})
	}
}

func TestWorkspaceRelayResumeReleasesAcknowledgedFrames(t *testing.T) {
	t.Parallel()
	const frameBytes = int64(100)
	for _, test := range []struct {
		name       string
		lastSeq    int64
		wantKept   []int64
		wantMemory int64
	}{
		{name: "nothing seen", lastSeq: 0, wantKept: []int64{1, 2, 3}, wantMemory: 3 * frameBytes},
		{name: "first frame seen", lastSeq: 1, wantKept: []int64{2, 3}, wantMemory: 2 * frameBytes},
		{name: "all but the last seen", lastSeq: 2, wantKept: []int64{3}, wantMemory: frameBytes},
		{name: "everything seen", lastSeq: 3, wantKept: []int64{}, wantMemory: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			relay := New(Deps{MemoryBytes: 1 << 20})
			stream := &Stream{
				ID: "relayconn_old:1", Channel: workspacesession.ChannelFiles, ConnectionID: "relayconn_old",
				PrincipalID: "principal", SessionID: "session", toPerson: 3, detachedAt: time.Now(),
				bufferBytes: 3 * frameBytes,
			}
			for seq := int64(1); seq <= 3; seq++ {
				stream.buffer = append(stream.buffer, BufferedFrame{seq: seq, bytes: frameBytes})
			}
			relay.memory = 3 * frameBytes
			relay.rooms["ws_1"] = &Room{
				WorkspaceID: "ws_1", people: map[string]*Connection{},
				Streams: map[string]*Stream{stream.ID: stream}, detached: map[string]*Stream{},
				terminals: map[string]*TerminalStream{},
			}
			relay.DetachPerson(t.Context(), &Connection{ID: stream.ConnectionID, WorkspaceID: "ws_1"}, time.Now())
			connection := &Connection{ID: "relayconn_new", WorkspaceID: "ws_1", PrincipalID: "principal", SessionID: "session"}
			resumed, replay, code := relay.ResumeStream(connection, stream.ID, test.lastSeq, time.Now())
			if code != "" || resumed != stream {
				t.Fatalf("resume = %v, %q", resumed, code)
			}
			if len(replay) != len(test.wantKept) {
				t.Fatalf("replayed %d frames, want %d", len(replay), len(test.wantKept))
			}
			kept := []int64{}
			for _, buffered := range stream.buffer {
				kept = append(kept, buffered.seq)
			}
			if !slices.Equal(kept, test.wantKept) {
				t.Fatalf("buffer holds %v, want %v", kept, test.wantKept)
			}
			if stream.bufferBytes != test.wantMemory || relay.memory != test.wantMemory {
				t.Fatalf("stream bytes = %d, relay memory = %d, want %d", stream.bufferBytes, relay.memory, test.wantMemory)
			}
			relay.Acknowledge(stream, test.lastSeq)
			if relay.memory != test.wantMemory {
				t.Fatalf("an ack of last_seq changed memory to %d", relay.memory)
			}
			relay.Acknowledge(stream, 3)
			if len(stream.buffer) != 0 || stream.bufferBytes != 0 || relay.memory != 0 {
				t.Fatalf("after the final ack: buffer %d, stream bytes %d, relay memory %d", len(stream.buffer), stream.bufferBytes, relay.memory)
			}
		})
	}
}

func TestWorkspaceRelayBeginExecRunRequiresTheServingRunner(t *testing.T) {
	t.Parallel()
	current := &Connection{ID: "relayconn_current", WorkspaceID: "ws_1", Runner: true}
	replaced := &Connection{ID: "relayconn_old", WorkspaceID: "ws_1", Runner: true}
	stream := &Stream{ID: "relayhub:1", Channel: workspacesession.ChannelExec, ConnectionID: HubStreamOwner, HubOwned: true}
	tests := []struct {
		name      string
		roomless  bool
		attached  *Connection
		streamID  string
		expecting *Connection
		want      bool
	}{
		{name: "the runner that was looked up", attached: current, streamID: stream.ID, expecting: current, want: true},
		{name: "any attached runner", attached: current, streamID: stream.ID, want: true},
		{name: "a runner replaced since the lookup", attached: current, streamID: stream.ID, expecting: replaced},
		{name: "a runner that detached", streamID: stream.ID, expecting: current},
		{name: "no runner at all", streamID: stream.ID},
		{name: "a stream closed since it was opened", attached: current, streamID: "relayhub:2", expecting: current},
		{name: "a workspace no longer served", roomless: true, streamID: stream.ID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			relay := New(Deps{})
			if !test.roomless {
				relay.rooms["ws_1"] = &Room{
					WorkspaceID: "ws_1", Runner: test.attached,
					people:   map[string]*Connection{},
					Streams:  map[string]*Stream{stream.ID: stream},
					detached: map[string]*Stream{},
					execRuns: map[string]*ExecRun{},
				}
			}
			if got := relay.BeginExecRun("ws_1", test.streamID, &ExecRun{RunID: "actionrun_1", ActionID: "action_1"}, test.expecting); got != test.want {
				t.Fatalf("beginExecRun = %v, want %v", got, test.want)
			}
			if test.roomless {
				return
			}
			if _, recorded := relay.rooms["ws_1"].execRuns[test.streamID]; recorded != test.want {
				t.Fatalf("run recorded = %v, want %v", recorded, test.want)
			}
		})
	}
}

func TestWorkspaceRelayDetachPersonClosesItsExecStreamsOnTheRunner(t *testing.T) {
	t.Parallel()
	runner := &Connection{ID: "relayconn_runner", WorkspaceID: "ws_1", Runner: true,
		Out: make(chan Outbound, 4), Done: make(chan struct{})}
	person := &Connection{ID: "relayconn_person", WorkspaceID: "ws_1"}
	exec := &Stream{ID: "relayconn_person:1", Channel: workspacesession.ChannelExec, ConnectionID: person.ID}
	files := &Stream{ID: "relayconn_person:2", Channel: workspacesession.ChannelFiles, ConnectionID: person.ID}
	preview := &Stream{ID: "relayconn_person:3", Channel: workspacesession.ChannelPreview, ConnectionID: person.ID}
	relay := New(Deps{})
	relay.rooms["ws_1"] = &Room{
		WorkspaceID: "ws_1", Runner: runner,
		people:   map[string]*Connection{person.ID: person},
		Streams:  map[string]*Stream{exec.ID: exec, files.ID: files, preview.ID: preview},
		detached: map[string]*Stream{},
		execRuns: map[string]*ExecRun{},
	}

	now := time.Now()
	relay.DetachPerson(t.Context(), person, now)

	room := relay.rooms["ws_1"]
	if _, parked := room.detached[exec.ID]; parked {
		t.Fatal("the exec stream was parked for a resume, want it closed")
	}
	if _, parked := room.detached[files.ID]; !parked {
		t.Fatal("the files stream was not parked for a resume")
	}
	select {
	case sent := <-runner.Out:
		if sent.Frame.Type != workspacesession.TypeClose || sent.Frame.Stream != exec.ID {
			t.Fatalf("runner received %+v, want a close of the exec stream", sent.Frame)
		}
	default:
		t.Fatal("the runner was not told to close the exec stream")
	}
	select {
	case sent := <-runner.Out:
		t.Fatalf("runner received an extra frame %+v", sent.Frame)
	default:
	}
	relay.DetachRunner(t.Context(), runner)
	if count := relay.SweepPage(t.Context(), now, 1); count != 0 || !relay.Active() {
		t.Fatalf("resumable streams lost before expiry: count=%d", count)
	}
	for _, left := range []int{1, 0} {
		count := relay.SweepPage(t.Context(), now.Add(workspacesession.ResumeWindow), 1)
		if count != 1 || relay.DetachedCount(person.WorkspaceID) != left || relay.Active() != (left > 0) {
			t.Fatalf("after sweeping one detached stream: count=%d remaining=%d active=%v", count, relay.DetachedCount(person.WorkspaceID), relay.Active())
		}
	}
}
