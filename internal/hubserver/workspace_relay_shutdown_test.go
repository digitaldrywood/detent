package hubserver

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestWorkspaceRelayShutdownJoinsConnectionFinalization(t *testing.T) {
	for _, state := range []string{"before attachment", "attached person", "attached runner", "detached person"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service := &workspaceService{}
				relay := newWorkspaceRelay(service)
				connection := &relayConnection{id: "connection", workspaceID: "workspace", done: make(chan struct{})}
				ctx, finished, err := relay.startConnection(t.Context(), connection)
				if err != nil {
					t.Fatal(err)
				}
				switch state {
				case "attached person", "detached person":
					if err := relay.attachPerson(connection); err != nil {
						finished()
						t.Fatal(err)
					}
					if state == "detached person" {
						relay.detachPerson(t.Context(), connection, time.Time{})
					}
				case "attached runner":
					connection.runner = true
					if _, err := relay.attachRunner(connection); err != nil {
						finished()
						t.Fatal(err)
					}
				}
				finalizing := make(chan struct{})
				release := make(chan struct{})
				finalized := make(chan struct{})
				releaseFinalization := func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}
				defer releaseFinalization()
				go func() {
					defer finished()
					<-ctx.Done()
					close(finalizing)
					<-release
					close(finalized)
				}()
				stopped := make(chan struct{})
				go func() {
					relay.stop(t.Context())
					close(stopped)
				}()
				<-finalizing
				synctest.Wait()
				select {
				case <-stopped:
					t.Error("relay shutdown returned before connection finalization")
				default:
				}
				if _, finishLate, err := relay.startConnection(t.Context(), &relayConnection{}); err == nil {
					finishLate()
					t.Error("relay shutdown admitted another connection owner")
				}
				select {
				case <-connection.done:
				default:
					t.Error("relay shutdown did not close the admitted connection")
				}
				releaseFinalization()
				<-stopped
				select {
				case <-finalized:
				default:
					t.Error("relay shutdown did not join connection finalization")
				}
			})
		})
	}
}

func TestWorkspaceRelayShutdownClosesUnownedAttachments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		relay := newWorkspaceRelay(&workspaceService{})
		person := &relayConnection{id: "person", workspaceID: "workspace", done: make(chan struct{})}
		runner := &relayConnection{id: "runner", workspaceID: "workspace", runner: true, done: make(chan struct{})}
		if err := relay.attachPerson(person); err != nil {
			t.Fatal(err)
		}
		if _, err := relay.attachRunner(runner); err != nil {
			t.Fatal(err)
		}
		relay.stop(t.Context())
		for _, connection := range []*relayConnection{person, runner} {
			select {
			case <-connection.done:
			default:
				t.Errorf("relay shutdown did not close %s", connection.id)
			}
		}
	})
}
