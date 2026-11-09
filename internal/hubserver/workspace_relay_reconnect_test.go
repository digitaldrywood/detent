package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func newWorkspaceRelayReconnectFixture(t *testing.T, frames int) (*workspaceService, *relayConnection, *relayConnection, *relayStream) {
	t.Helper()
	now := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
	service := &workspaceService{
		server: &Service{config: Config{now: func() time.Time { return now }}},
		config: WorkspaceConfig{RelayMemoryBytes: 1 << 20},
	}
	service.relay = newWorkspaceRelay(service)
	runner := &relayConnection{id: "relayrunner", workspaceID: "ws_1", runner: true, out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{})}
	person := &relayConnection{id: "relayperson_new", workspaceID: "ws_1", principalID: "principal", sessionID: "session", out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{})}
	if _, err := service.relay.attachRunner(runner); err != nil {
		t.Fatal(err)
	}
	if err := service.relay.attachPerson(person); err != nil {
		t.Fatal(err)
	}
	stream := &relayStream{
		id: "relayperson_old:1", channel: workspacesession.ChannelFiles, connectionID: "relayperson_old",
		principalID: person.principalID, sessionID: person.sessionID, detachedAt: now,
	}
	service.relay.rooms[runner.workspaceID].detached[stream.id] = stream
	frame := workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeFilesListed}
	for range frames {
		if _, accepted := service.relay.captureRunnerFrame(runner, stream, frame, relayRunnerOutput{}); !accepted {
			t.Fatal("detached stream did not capture its replay frame")
		}
	}
	return service, runner, person, stream
}

func TestWorkspaceRelayRunnerFrameUsesResumedAudience(t *testing.T) {
	for _, source := range []string{"detached lookup", "previous person lookup"} {
		t.Run(source, func(t *testing.T) {
			service, runner, person, stream := newWorkspaceRelayReconnectFixture(t, 1)
			relay := service.relay
			var previous *relayConnection
			if source == "previous person lookup" {
				previous = &relayConnection{id: stream.connectionID, workspaceID: runner.workspaceID, principalID: person.principalID, sessionID: person.sessionID, out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{}), streams: 1}
				room := relay.rooms[runner.workspaceID]
				room.people[previous.id] = previous
				delete(room.detached, stream.id)
				room.streams[stream.id] = stream
			}
			resolved, audience := relay.runnerStream(runner, stream.id)
			if resolved != stream || audience != previous {
				t.Fatal("runner lookup did not capture its previous audience")
			}
			if previous != nil {
				previous.close("client_closed")
				relay.detachPerson(t.Context(), previous, service.now())
			}
			if resumed, replay, code := relay.resumeStream(person, stream.id, 1, service.now()); resumed != stream || len(replay) != 0 || code != "" {
				t.Fatalf("resume=%+v replay=%+v code=%q", resumed, replay, code)
			}
			select {
			case outbound := <-person.out:
				if outbound.frame.Type != workspacesession.TypeResumed || outbound.frame.Stream != stream.id {
					t.Errorf("resumed prefix=%+v", outbound)
				}
			default:
				t.Error("resumed audience did not receive its resume prefix")
			}
			frame := workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeFilesListed}
			service.handleRunnerStreamFrame(t.Context(), runner, resolved, audience, frame)
			select {
			case outbound := <-person.out:
				if outbound.frame.Type != frame.Type || outbound.frame.Stream != stream.id || outbound.frame.Seq != 2 {
					t.Errorf("resumed audience received=%+v", outbound)
				}
			default:
				t.Error("runner frame was not delivered to the resumed audience")
			}
		})
	}
}

func TestWorkspaceRelayResumeRejectsReleasedReplayPrefix(t *testing.T) {
	for _, test := range []struct {
		name       string
		frames     int
		ackThrough int64
		lastSeq    int64
		wantCode   string
		wantReplay int
	}{
		{name: "released prefix is missing", frames: 2, ackThrough: 1, wantCode: workspacesession.CodeResumeFailed},
		{name: "resume at acknowledged prefix", frames: 2, ackThrough: 1, lastSeq: 1, wantReplay: 1},
		{name: "everything received", frames: 2, ackThrough: 1, lastSeq: 2},
		{name: "negative sequence", frames: 2, ackThrough: 1, lastSeq: -1, wantCode: workspacesession.CodeResumeFailed},
		{name: "future sequence", frames: 2, ackThrough: 1, lastSeq: 3, wantCode: workspacesession.CodeResumeFailed},
		{name: "caught up at zero"},
		{name: "caught up after excessive acknowledgement", frames: 2, ackThrough: 3, lastSeq: 2},
		{name: "missing frame after excessive acknowledgement", frames: 2, ackThrough: 3, lastSeq: 1, wantCode: workspacesession.CodeResumeFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runner, person, stream := newWorkspaceRelayReconnectFixture(t, test.frames)
			relay := service.relay
			relay.acknowledge(runner.workspaceID, stream, test.ackThrough)
			before := relay.memoryUsed()
			resumed, replay, code := relay.resumeStream(person, stream.id, test.lastSeq, service.now())
			if code != test.wantCode {
				t.Errorf("resume code=%q, want %q", code, test.wantCode)
			}
			if test.wantCode != "" {
				if resumed != nil || len(replay) != 0 || person.streams != 0 || relay.rooms[runner.workspaceID].detached[stream.id] != stream || relay.memoryUsed() != before {
					t.Errorf("refused resume changed ownership or buffering: stream=%+v replay=%+v held=%d buffered=%d", resumed, replay, person.streams, relay.memoryUsed())
				}
				return
			}
			if resumed != stream || len(replay) != test.wantReplay || person.streams != 1 {
				t.Errorf("resume=%+v replay=%+v held=%d", resumed, replay, person.streams)
			}
			if len(replay) != 0 && replay[0].Seq != 2 {
				t.Errorf("replayed frame=%+v, want sequence 2", replay[0])
			}
		})
	}
}

func TestWorkspaceRelayResumeQueuesReplayBeforeLiveFrames(t *testing.T) {
	service, runner, person, stream := newWorkspaceRelayReconnectFixture(t, 1)
	if resumed, replay, code := service.relay.resumeStream(person, stream.id, 0, service.now()); resumed != stream || len(replay) != 1 || code != "" {
		t.Fatalf("resume=%+v replay=%+v code=%q", resumed, replay, code)
	}
	service.handleRunnerFrame(t.Context(), runner, workspacesession.Frame{Channel: stream.channel, Stream: stream.id, Type: workspacesession.TypeFilesListed})
	for index, expected := range []struct {
		frameType string
		sequence  int64
	}{
		{frameType: workspacesession.TypeResumed},
		{frameType: workspacesession.TypeFilesListed, sequence: 1},
		{frameType: workspacesession.TypeFilesListed, sequence: 2},
	} {
		select {
		case outbound := <-person.out:
			if outbound.frame.Type != expected.frameType || outbound.frame.Seq != expected.sequence || outbound.frame.Stream != stream.id {
				t.Errorf("resume frame %d=%+v, want %s sequence %d", index, outbound, expected.frameType, expected.sequence)
			}
		default:
			t.Errorf("resume frame %d missing, want %s sequence %d", index, expected.frameType, expected.sequence)
		}
	}
}

func TestWorkspaceRelayResumeOverflowKeepsReplayUntilAcknowledged(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity int
		prefill  bool
	}{
		{name: "resume prefix overflow", capacity: 1, prefill: true},
		{name: "replay overflow", capacity: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runner, person, stream := newWorkspaceRelayReconnectFixture(t, 4)
			person.out = make(chan relayOutbound, test.capacity)
			if test.prefill {
				person.out <- relayOutbound{frame: workspacesession.Frame{Type: workspacesession.TypeAck}}
			}
			before := service.relay.memoryUsed()
			resumed, replay, code := service.relay.resumeStream(person, stream.id, 1, service.now())
			if resumed != stream || len(replay) != 3 || code != "" {
				t.Errorf("overflow resume=%+v replay=%+v code=%q", resumed, replay, code)
			}
			select {
			case <-person.done:
				if reason := person.reason(); reason != workspacesession.CodeOverflow {
					t.Errorf("resume overflow closed with reason %q", reason)
				}
			default:
				t.Error("resume overflow left the connection open")
			}
			if stream.ackedThrough != 1 || len(stream.buffer) != 3 || stream.buffer[0].seq != 2 || stream.buffer[2].seq != 4 || service.relay.memoryUsed() != before-64 {
				t.Errorf("resume overflow changed unacknowledged replay: ack=%d buffer=%+v memory=%d", stream.ackedThrough, stream.buffer, service.relay.memoryUsed())
			}
			if service.relay.rooms[runner.workspaceID].streams[stream.id] != stream {
				t.Error("resume overflow removed the stream before connection teardown")
			}
		})
	}
}
