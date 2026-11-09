package hubserver

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func addWorkspaceRelaySweepStream(relay *workspaceRelay, workspace string, detachedAt time.Time) (*relayRoom, *relayStream) {
	room := relay.ensureRoom(workspace)
	room.runner = &relayConnection{id: "runner", workspaceID: workspace, runner: true, out: make(chan relayOutbound, 1), done: make(chan struct{})}
	frame := workspacesession.Frame{Channel: workspacesession.ChannelFiles, Stream: workspacesession.StreamID("person", 1), Type: workspacesession.TypeFilesListed, Seq: 1}
	stream := &relayStream{
		id: frame.Stream, channel: frame.Channel, connectionID: "person", principalID: "principal", sessionID: "session",
		detachedAt: detachedAt, toPerson: 1, bufferBytes: 64,
		buffer: []relayBufferedFrame{{seq: 1, frame: frame, bytes: 64}},
	}
	room.detached[stream.id] = stream
	relay.memory += stream.bufferBytes
	return room, stream
}

func TestWorkspaceRelaySweepUsesBoundedRoomPage(t *testing.T) {
	relay := newWorkspaceRelay(&workspaceService{})
	started := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
	for index := range 101 {
		addWorkspaceRelaySweepStream(relay, fmt.Sprintf("workspace_%03d", index), started)
	}
	now := started.Add(workspacesession.ResumeWindow)
	relay.sweep(t.Context(), now)
	closed := 0
	var pending *relayRoom
	for _, room := range relay.rooms {
		if len(room.detached) == 1 {
			if pending != nil {
				t.Fatal("bounded sweep left more than one pending room")
			}
			pending = room
		}
		select {
		case outbound := <-room.runner.out:
			if outbound.frame.Type != workspacesession.TypeClose || outbound.frame.Stream != workspacesession.StreamID("person", 1) {
				t.Errorf("expired stream close=%+v", outbound)
			}
			closed++
		default:
		}
	}
	if closed != 100 || relay.memoryUsed() != 64 {
		t.Errorf("first sweep closed=%d buffered=%d, want 100 and 64", closed, relay.memoryUsed())
	}
	if pending == nil {
		t.Fatal("first sweep consumed every room instead of retaining its bounded continuation")
	}
	connection := &relayConnection{id: "resumed", workspaceID: pending.workspaceID, principalID: "principal", sessionID: "session"}
	if _, _, code := relay.resumeStream(connection, workspacesession.StreamID("person", 1), 0, now); code != workspacesession.CodeResumeFailed {
		t.Errorf("unswept expired stream resumed with code %q", code)
	}
	relay.sweep(t.Context(), now)
	if len(pending.detached) != 0 || relay.memoryUsed() != 0 {
		t.Errorf("second sweep left detached=%d buffered=%d", len(pending.detached), relay.memoryUsed())
	}
	select {
	case outbound := <-pending.runner.out:
		if outbound.frame.Type != workspacesession.TypeClose {
			t.Errorf("continued sweep close=%+v", outbound)
		}
	default:
		t.Error("continued sweep did not close the pending stream")
	}
}

func TestWorkspaceRelaySweepKeepsRoomCursorAfterDeletion(t *testing.T) {
	for _, count := range []int{101, 102} {
		for _, removal := range []string{"workspace close", "person detach", "runner detach"} {
			t.Run(fmt.Sprintf("%d/%s", count, removal), func(t *testing.T) {
				relay := newWorkspaceRelay(&workspaceService{})
				started := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
				for index := range 100 {
					addWorkspaceRelaySweepStream(relay, fmt.Sprintf("workspace_%03d", index), started)
				}
				target := relay.ensureRoom("workspace_target")
				connection := &relayConnection{id: "connection", workspaceID: target.workspaceID, out: make(chan relayOutbound, 1), done: make(chan struct{})}
				if removal == "person detach" {
					target.people[connection.id] = connection
				} else {
					connection.runner = true
					target.runner = connection
				}
				if count == 102 {
					addWorkspaceRelaySweepStream(relay, "workspace_after_target", started)
				}
				now := started.Add(workspacesession.ResumeWindow)
				relay.sweep(t.Context(), now)
				switch removal {
				case "workspace close":
					relay.closeWorkspace(t.Context(), target.workspaceID, workspacesession.CodeWorkspaceClosed)
				case "person detach":
					relay.detachPerson(t.Context(), connection, now)
				case "runner detach":
					relay.detachRunner(t.Context(), connection)
				}
				if relay.rooms[target.workspaceID] != nil {
					t.Fatal("cursor target was not removed")
				}
				recreated, _ := addWorkspaceRelaySweepStream(relay, target.workspaceID, started)
				if recreated == target {
					t.Fatal("workspace recreation reused the removed room")
				}
				relay.sweep(t.Context(), now)
				wantBuffered := int64(0)
				if count == 101 {
					wantBuffered = 64
				}
				if buffered := relay.memoryUsed(); buffered != wantBuffered {
					t.Errorf("sweep after cursor deletion buffered=%d, want %d", buffered, wantBuffered)
				}
				relay.sweep(t.Context(), now)
				if len(recreated.detached) != 0 || relay.memoryUsed() != 0 {
					t.Errorf("recreated room was lost from sweep continuation: detached=%d buffered=%d", len(recreated.detached), relay.memoryUsed())
				}
			})
		}
	}
}

func TestWorkspaceRelaySweepPreservesResumeWindow(t *testing.T) {
	for _, test := range []struct {
		name        string
		offset      time.Duration
		wantExpired bool
	}{
		{name: "before boundary", offset: -time.Nanosecond},
		{name: "at boundary", wantExpired: true},
		{name: "after boundary", offset: time.Nanosecond, wantExpired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			relay := newWorkspaceRelay(&workspaceService{})
			started := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
			room, stream := addWorkspaceRelaySweepStream(relay, "workspace", started)
			now := started.Add(workspacesession.ResumeWindow + test.offset)
			relay.sweep(t.Context(), now)
			connection := &relayConnection{id: "resumed", workspaceID: room.workspaceID, principalID: stream.principalID, sessionID: stream.sessionID, out: make(chan relayOutbound, relayWriteQueue), done: make(chan struct{})}
			resumed, replay, code := relay.resumeStream(connection, stream.id, 0, now)
			if !test.wantExpired {
				if resumed != stream || len(replay) != 1 || code != "" || relay.memoryUsed() != 64 {
					t.Errorf("live resume=%+v replay=%+v code=%q buffered=%d", resumed, replay, code, relay.memoryUsed())
				}
				select {
				case outbound := <-room.runner.out:
					t.Errorf("sweep closed a resumable stream: %+v", outbound)
				default:
				}
				return
			}
			if resumed != nil || code != workspacesession.CodeResumeFailed || len(room.detached) != 0 || relay.memoryUsed() != 0 {
				t.Errorf("expired resume=%+v code=%q detached=%d buffered=%d", resumed, code, len(room.detached), relay.memoryUsed())
			}
			select {
			case outbound := <-room.runner.out:
				if outbound.frame.Type != workspacesession.TypeClose || outbound.frame.Stream != stream.id {
					t.Errorf("expired close=%+v", outbound)
				}
			default:
				t.Error("expired stream was not closed on the runner")
			}
		})
	}
}

func TestWorkspaceRelaySweepReleasesRunnerlessRoom(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(fmt.Sprintf("rooms-%d", count), func(t *testing.T) {
			relay := newWorkspaceRelay(&workspaceService{})
			started := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
			var target *relayRoom
			for index := range count {
				room, _ := addWorkspaceRelaySweepStream(relay, fmt.Sprintf("workspace_%03d", index), started)
				if index == count/2 {
					target = room
				}
			}
			if !relay.detachRunner(t.Context(), target.runner) || relay.rooms[target.workspaceID] != target || target.runner != nil || len(target.detached) != 1 {
				t.Fatal("runner detach did not preserve the final detached stream")
			}
			assertReleased := func(phase string) {
				t.Helper()
				if room := relay.rooms[target.workspaceID]; room != nil {
					t.Errorf("%s retained the runnerless room after stream expiry", phase)
				}
				if len(relay.rooms) != count-1 || relay.memoryUsed() != 0 {
					t.Errorf("%s left rooms=%d buffered=%d, want %d and zero", phase, len(relay.rooms), relay.memoryUsed(), count-1)
				}
			}
			now := started.Add(workspacesession.ResumeWindow)
			relay.sweep(t.Context(), now)
			assertReleased("first sweep")
			recreated, _ := addWorkspaceRelaySweepStream(relay, target.workspaceID, started)
			if recreated == target {
				t.Fatal("room recreation reused the expired room")
			}
			if !relay.detachRunner(t.Context(), recreated.runner) {
				t.Fatal("recreated runner did not detach")
			}
			relay.sweep(t.Context(), now)
			assertReleased("recreated room sweep")
		})
	}
}
