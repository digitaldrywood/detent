package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The relay (decisions section 18.2).
//
// The hub forwards frames between a person's connection and the runner's
// connection for one workspace, with its own checks in the middle. There may be
// many person connections (tabs, people) and exactly one runner connection per
// workspace.
//
// Three rules shape every structure here:
//
//   - A stream belongs to the person connection that opened it and only that
//     connection sees its frames, which is why the stream id carries the
//     connection and why routing is a map lookup rather than a broadcast.
//   - Backpressure never crosses into control delivery: the runner keeps the
//     relay and the control long-poll on separate connections and goroutines,
//     so a wedged relay writer must never block anything but itself. Every
//     write here goes through a bounded channel a writer goroutine drains, and
//     a connection that cannot keep up is closed rather than waited on.
//   - Authority is re-validated on every person-originated frame, not cached
//     from the upgrade. A socket that lives for an hour would otherwise
//     outlive the grant that opened it.

const (
	// relayWriteQueue is how many frames a connection may have pending on its
	// writer before it is closed. It is small: the byte budget that actually
	// bounds a stream is StreamBufferBytes, and this only stops a scheduling
	// hiccup from becoming a queue.
	relayWriteQueue = 64
	// relayWriteTimeout bounds one socket write. A peer that cannot take a
	// frame in this long is gone, whatever its TCP state claims.
	relayWriteTimeout = 10 * time.Second
	// relayReadLimit is the largest message the hub will read. It is the
	// frame cap plus room for the JSON envelope around it, so a legal
	// maximum-size payload is never refused by the transport.
	relayReadLimit = workspacesession.MaxFrameBytes + (16 << 10)
)

// workspaceRelay holds every live connection of every workspace on this hub.
type workspaceRelay struct {
	service *workspaceService
	mu      sync.Mutex
	// rooms is keyed by workspace id. A workspace with no connection has no
	// room, so an idle hub holds nothing.
	rooms map[string]*relayRoom
	// memory is the total buffered bytes across every stream, the budget
	// workspaces.relay.memory caps.
	memory  int64
	stopped bool
}

// relayRoom is one workspace's live connections and streams.
type relayRoom struct {
	workspaceID string
	runner      *relayConnection
	people      map[string]*relayConnection
	streams     map[string]*relayStream
	// detached holds streams whose person connection dropped, waiting for a
	// resume. They still count against the workspace's stream limit, because
	// the runner is still holding whatever they opened.
	detached map[string]*relayStream
	// terminals is the recording of each open terminal stream, keyed by stream
	// id (section 18.3). The hub is not only forwarding a terminal stream, it
	// is recording it, and a recording assembled anywhere but here would be a
	// recording of something other than what actually crossed the relay.
	terminals map[string]*terminalStreamState
}

// terminalStreamState is one terminal stream being recorded, held in memory
// while the stream lives (decisions section 18.3).
//
// A shell writes thousands of
// spans and a database write per span would make the relay the slowest part of
// looking at one. Nothing is lost by waiting, because every path that ends a
// stream writes what it collected -- that is what finishTerminalRecordings is
// for.
//
// The buffer is bounded by MaxRecordingBytes, so one shell holds at most that
// much of the hub's memory whatever it prints. That is a separate budget from
// the relay's own StreamBufferBytes: that one bounds unacknowledged frames
// waiting for a person, and releasing it on an ack says nothing about how much
// of the session the hub still has to store.
type terminalStreamState struct {
	id             string
	workspaceID    string
	organizationID string
	projectID      string
	relaySessionID string
	streamID       string
	principalID    string
	subject        string
	isolation      string
	supportReason  string
	startedAt      time.Time
	cols           int
	rows           int
	recording      *workspacesession.Recording
}

// relayStream is one logical exchange on one channel.
type relayStream struct {
	id      string
	channel string
	// connectionID is the person connection that owns the stream. It changes
	// on a resume, which is the only time a stream moves between connections.
	connectionID string
	// principalID and sessionID are who opened it. A resume must match both:
	// a stream may be resumed only by a connection whose principal and
	// session match the ones that opened it.
	principalID string
	sessionID   string
	// toRunner and toPerson are the per-direction sequences, starting at 1.
	toRunner int64
	toPerson int64
	// buffer holds runner-bound-to-person frames the person has not
	// acknowledged, so a resume can replay them.
	buffer      []relayBufferedFrame
	bufferBytes int64
	// ackedThrough is the highest person-bound sequence acknowledged.
	ackedThrough int64
	// detachedAt is when the owning connection dropped; a stream past
	// ResumeWindow from it is gone.
	detachedAt time.Time
}

// relayOutbound is one queued write. final carries the reason the connection
// ends after this frame, which is how a close notice actually reaches the peer:
// closing the connection first would drop the frame that explains why.
type relayOutbound struct {
	frame workspacesession.Frame
	final string
}

// relayBufferedFrame is one replayable frame.
type relayBufferedFrame struct {
	seq   int64
	frame workspacesession.Frame
	bytes int64
}

// relayConnection is one socket, person or runner.
type relayConnection struct {
	id          string
	workspaceID string
	socket      *websocket.Conn
	runner      bool
	actor       workspacesession.Actor
	principalID string
	sessionID   string
	// supportReason is why a support actor is on this connection. Section 18.3
	// lets a support session have a terminal only with a reason recorded and
	// only at container isolation, so the reason is held here rather than
	// re-read per frame: the audit row already carries it, a recording carries
	// it, and the per-frame gate has to consult it on every open.
	supportReason string
	// hostedRole is the membership role this connection authenticated with. A
	// user-isolation terminal is owner-or-admin only (section 18.3), and the
	// role a socket opened with is the one it keeps: a role change emits
	// authority.changed, which closes every connection of that principal, so a
	// stale role cannot outlive the change that made it stale.
	hostedRole string
	// sessionHash is the hosted session row this connection authenticated
	// with. The per-frame re-check reads it directly rather than re-parsing a
	// cookie, so a revoked session closes the socket on its next frame.
	sessionHash string
	subject     string
	// out is the writer's queue. Nothing outside the writer goroutine ever
	// touches the socket's write side.
	out chan relayOutbound
	// done closes when the connection is finished; closeReason says why.
	done        chan struct{}
	closeOnce   sync.Once
	closeReason string
	// streams counts the streams this connection currently holds, for the
	// per-connection limit. It falls when a stream is closed, so a reader that
	// tidies up after itself keeps its budget; streamSeq is what names them,
	// and it only ever rises, so a closed id is never handed out twice.
	streams   int
	streamSeq int
	bytesIn   int64
	bytesOut  int64
	// auditID is the workspace_relay_sessions row this connection writes.
	auditID string
	mu      sync.Mutex
}

func newWorkspaceRelay(service *workspaceService) *workspaceRelay {
	return &workspaceRelay{service: service, rooms: map[string]*relayRoom{}}
}

// stop closes every connection. It is called when the hub shuts down, so a
// person sees a close rather than a socket that simply stops answering.
func (r *workspaceRelay) stop(ctx context.Context) {
	r.mu.Lock()
	r.stopped = true
	connections := []*relayConnection{}
	for _, room := range r.rooms {
		if room.runner != nil {
			connections = append(connections, room.runner)
		}
		for _, person := range room.people {
			connections = append(connections, person)
		}
	}
	recorded := []*terminalStreamState{}
	for _, room := range r.rooms {
		recorded = append(recorded, room.takeTerminalRecordings(nil)...)
	}
	r.rooms = map[string]*relayRoom{}
	r.memory = 0
	r.mu.Unlock()
	// A terminal in flight at shutdown: nothing will ever add
	// another event to its recording, so what was collected is written now
	// rather than lost with the process (section 18.3).
	r.service.finishTerminalRecordings(ctx, recorded)
	for _, connection := range connections {
		connection.close("server_shutdown")
	}
}

// ensureRoom returns the workspace's room, creating it when there is none. It
// never returns nil, which is why it is a separate function from the lookups
// that may: a boolean parameter deciding whether a result can be nil is a
// parameter every caller has to remember, and every reader has to check.
func (r *workspaceRelay) ensureRoom(workspaceID string) *relayRoom {
	if room := r.rooms[workspaceID]; room != nil {
		return room
	}
	room := &relayRoom{
		workspaceID: workspaceID,
		people:      map[string]*relayConnection{},
		streams:     map[string]*relayStream{},
		detached:    map[string]*relayStream{},
		terminals:   map[string]*terminalStreamState{},
	}
	r.rooms[workspaceID] = room
	return room
}

// closeWorkspace ends every connection of a workspace with one reason. The
// workspace service calls it when a workspace reaches a terminal state, goes
// unreachable or is re-requested: in all three the runner can no longer serve
// what a person is holding, and saying so is better than leaving them waiting
// on frames that will never come.
func (r *workspaceRelay) closeWorkspace(ctx context.Context, workspaceID, reason string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	recorded := room.takeTerminalRecordings(nil)
	delete(r.rooms, workspaceID)
	connections := []*relayConnection{}
	if room.runner != nil {
		connections = append(connections, room.runner)
	}
	for _, person := range room.people {
		connections = append(connections, person)
	}
	for _, stream := range room.streams {
		r.memory -= stream.bufferBytes
	}
	for _, stream := range room.detached {
		r.memory -= stream.bufferBytes
	}
	if r.memory < 0 {
		r.memory = 0
	}
	r.mu.Unlock()
	r.service.finishTerminalRecordings(ctx, recorded)
	for _, connection := range connections {
		connection.sendFinal(workspacesession.ErrorFrame("", "", reason, "The workspace is no longer serving this connection"), reason)
	}
}

// sweep drops detached streams past the resume window and releases their
// buffers. The runner is told, so a PTY or an open read does not outlive the
// stream that asked for it.
func (r *workspaceRelay) sweep(ctx context.Context, now time.Time) {
	type expiry struct {
		room   *relayRoom
		stream *relayStream
	}
	r.mu.Lock()
	expired := []expiry{}
	recorded := []*terminalStreamState{}
	for _, room := range r.rooms {
		for id, stream := range room.detached {
			if now.Sub(stream.detachedAt) < workspacesession.ResumeWindow {
				continue
			}
			delete(room.detached, id)
			r.memory -= stream.bufferBytes
			// The resume window ran out, so the PTY behind this stream is
			// about to be killed by the close below and its recording is
			// complete (section 18.3).
			recorded = append(recorded, room.takeTerminalRecordings(func(streamID string) bool { return streamID == id })...)
			expired = append(expired, expiry{room: room, stream: stream})
		}
	}
	if r.memory < 0 {
		r.memory = 0
	}
	runners := map[*relayRoom]*relayConnection{}
	for _, item := range expired {
		runners[item.room] = item.room.runner
	}
	r.mu.Unlock()
	r.service.finishTerminalRecordings(ctx, recorded)
	for _, item := range expired {
		if runner := runners[item.room]; runner != nil {
			runner.send(workspacesession.Frame{Channel: item.stream.channel, Stream: item.stream.id, Type: workspacesession.TypeClose})
		}
	}
}

// attachPerson registers a person connection.
func (r *workspaceRelay) attachPerson(connection *relayConnection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return errors.New("the relay is shutting down")
	}
	room := r.ensureRoom(connection.workspaceID)
	room.people[connection.id] = connection
	return nil
}

// detachPerson removes a person connection and parks its streams for a resume.
// The streams are not closed: section 18.2 gives the person 60 seconds to come
// back with a new ticket, and the runner keeps whatever it opened for that long
// too, so tearing them down here would defeat the whole reconnect path.
func (r *workspaceRelay) detachPerson(ctx context.Context, connection *relayConnection, now time.Time) {
	r.mu.Lock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	if room.people[connection.id] == connection {
		delete(room.people, connection.id)
	}
	for id, stream := range room.streams {
		if stream.connectionID != connection.id {
			continue
		}
		delete(room.streams, id)
		stream.detachedAt = now
		room.detached[id] = stream
	}
	// A terminal recording is not taken here: a PTY survives its
	// connection dropping for sixty seconds precisely so the person can come
	// back to the same shell, and writing the recording now would either
	// truncate a session still in progress or leave two rows for one shell.
	// The sweep writes it when the window runs out, and a resume continues it.
	if len(room.people) == 0 && room.runner == nil && len(room.detached) == 0 {
		delete(r.rooms, connection.workspaceID)
	}
	r.mu.Unlock()
}

// attachRunner registers the workspace's one runner connection and reports the
// connection it replaced. A second runner connection for the same workspace
// replaces the first, which closes with superseded: two runners answering one
// workspace would each be serving a different worktree.
func (r *workspaceRelay) attachRunner(connection *relayConnection) (*relayConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, errors.New("the relay is shutting down")
	}
	room := r.ensureRoom(connection.workspaceID)
	previous := room.runner
	room.runner = connection
	return previous, nil
}

// detachRunner removes the runner connection when it is still the current one,
// and reports whether it was. A connection that had already been superseded
// detaches without taking anything with it: the workspace's runs belong to the
// runner that replaced it.
func (r *workspaceRelay) detachRunner(ctx context.Context, connection *relayConnection) bool {
	r.mu.Lock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		r.mu.Unlock()
		return false
	}
	current := room.runner == connection
	recorded := []*terminalStreamState{}
	if current {
		room.runner = nil
		// A terminal's PTY survives the runner's socket dropping for the resume
		// window (section 18.2), but this hub will never hear from it again on
		// this connection: a redial arrives as a new one and the runner's own
		// timer decides the shell's fate. So what was recorded is written now
		// rather than held for a continuation that cannot reach it.
		recorded = room.takeTerminalRecordings(nil)
	}
	if len(room.people) == 0 && room.runner == nil && len(room.detached) == 0 {
		delete(r.rooms, connection.workspaceID)
	}
	r.mu.Unlock()
	r.service.finishTerminalRecordings(ctx, recorded)
	return current
}

// runnerFor returns the workspace's runner connection, if one is attached.
func (r *workspaceRelay) runnerFor(workspaceID string) *relayConnection {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return nil
	}
	return room.runner
}

// openStream allocates a stream for a person connection, applying both stream
// limits and the relay memory cap.
func (r *workspaceRelay) openStream(connection *relayConnection, channel string) (*relayStream, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		return nil, workspacesession.CodeWorkspaceClosed
	}
	if r.memory >= r.service.config.RelayMemoryBytes {
		// The budget is already spent, so a new stream would have nowhere to
		// buffer. Refusing it is the only answer that does not put the hub
		// process at risk for one reader's convenience.
		return nil, workspacesession.CodeRelayBusy
	}
	if connection.streams >= workspacesession.MaxStreamsPerConnection {
		return nil, workspacesession.CodeStreamLimit
	}
	if len(room.streams)+len(room.detached) >= workspacesession.MaxStreamsPerWorkspace {
		return nil, workspacesession.CodeStreamLimit
	}
	connection.streams++
	connection.streamSeq++
	stream := &relayStream{
		id: workspacesession.StreamID(connection.id, connection.streamSeq), channel: channel,
		connectionID: connection.id, principalID: connection.principalID, sessionID: connection.sessionID,
	}
	room.streams[stream.id] = stream
	return stream, ""
}

// channelStream returns the stream this connection already has open on a
// channel, or nil. It is what makes a request channel's stream-less frame a
// continuation rather than an allocation (section 18.2): files, diff and
// preview are request/response, so a client that never names a stream would
// otherwise spend its whole budget on one directory listing repeated eight
// times and could never list again.
//
// The lowest-numbered stream wins so the answer does not depend on map order.
// A connection can hold more than one stream on a channel only by resuming a
// parked one onto a connection that already had one, and both of those are
// usable; picking the same one every time is what matters.
func (r *workspaceRelay) channelStream(connection *relayConnection, channel string) *relayStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		return nil
	}
	var found *relayStream
	lowest := 0
	for id, stream := range room.streams {
		if stream.connectionID != connection.id || stream.channel != channel {
			continue
		}
		owner, index, err := workspacesession.ParseStreamID(id)
		if err != nil || owner != connection.id {
			continue
		}
		if found == nil || index < lowest {
			found, lowest = stream, index
		}
	}
	return found
}

// lookupStream resolves a stream a person frame names and proves this
// connection owns it. A connection that names another's stream is answered as
// if the stream did not exist, because it does not exist for that connection.
func (r *workspaceRelay) lookupStream(connection *relayConnection, id string) *relayStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		return nil
	}
	stream := room.streams[id]
	if stream == nil || stream.connectionID != connection.id {
		return nil
	}
	return stream
}

// runnerStream resolves a stream a runner frame names. The runner may address
// any stream of the workspace; it is the hub that decides which person sees it.
func (r *workspaceRelay) runnerStream(workspaceID, id string) (*relayStream, *relayConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return nil, nil
	}
	if stream := room.streams[id]; stream != nil {
		return stream, room.people[stream.connectionID]
	}
	// A detached stream still accepts runner output: it is buffered for the
	// resume the person has 60 seconds to make.
	if stream := room.detached[id]; stream != nil {
		return stream, nil
	}
	return nil, nil
}

// closeStream removes a stream, releases its buffer and gives the slot back to
// the connection that held it.
//
// Returning the slot is the half that is easy to forget and expensive to omit:
// without it a connection's budget only ever falls, so a person who closes
// every stream they open still runs out of them, and the count that refuses
// the next one is describing streams nobody is holding.
//
// A detached stream's connection is already gone, so there is nothing to credit
// it back to; its slot left with the socket.
func (r *workspaceRelay) closeStream(ctx context.Context, workspaceID, id string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	for _, set := range []map[string]*relayStream{room.streams, room.detached} {
		stream := set[id]
		if stream == nil {
			continue
		}
		delete(set, id)
		if owner := room.people[stream.connectionID]; owner != nil && owner.streams > 0 {
			owner.streams--
		}
		r.memory -= stream.bufferBytes
		if r.memory < 0 {
			r.memory = 0
		}
	}
	// A closed stream carries no more events, so its recording is
	// complete and is written with what it collected (section 18.3).
	recorded := room.takeTerminalRecordings(func(streamID string) bool { return streamID == id })
	r.mu.Unlock()
	r.service.finishTerminalRecordings(ctx, recorded)
}

// resumeStream moves a detached stream onto a new connection. It is the whole
// of section 18.2's resume rule: inside the window, same principal, same
// session, and a last_seq the hub can still replay from.
func (r *workspaceRelay) resumeStream(connection *relayConnection, id string, lastSeq int64, now time.Time) (*relayStream, []workspacesession.Frame, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.workspaceID]
	if room == nil {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	stream := room.detached[id]
	if stream == nil {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if now.Sub(stream.detachedAt) >= workspacesession.ResumeWindow {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if stream.principalID != connection.principalID || stream.sessionID != connection.sessionID {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if lastSeq < 0 || lastSeq > stream.toPerson {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	replay := []workspacesession.Frame{}
	for _, buffered := range stream.buffer {
		if buffered.seq > lastSeq {
			replay = append(replay, buffered.frame)
		}
	}
	if len(replay) == 0 && lastSeq < stream.toPerson {
		// The client is behind and the hub no longer holds what it missed:
		// answering resume_failed is the only honest reply, because a silent
		// gap in a stream is worse than a stream that ended.
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if connection.streams >= workspacesession.MaxStreamsPerConnection {
		return nil, nil, workspacesession.CodeStreamLimit
	}
	delete(room.detached, id)
	stream.connectionID = connection.id
	stream.detachedAt = time.Time{}
	stream.ackedThrough = lastSeq
	room.streams[id] = stream
	connection.streams++
	return stream, replay, ""
}

// bufferForPerson records a person-bound frame for replay and reports whether
// the stream overflowed. The cap is per stream per direction; beyond it the
// stream closes with overflow rather than the hub growing without bound for a
// reader who has stopped acknowledging.
func (r *workspaceRelay) bufferForPerson(workspaceID string, stream *relayStream, frame workspacesession.Frame, seq int64) bool {
	size := int64(len(frame.Payload)) + 64
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.buffer = append(stream.buffer, relayBufferedFrame{seq: seq, frame: frame, bytes: size})
	stream.bufferBytes += size
	r.memory += size
	if stream.bufferBytes > workspacesession.StreamBufferBytes {
		return true
	}
	if r.memory > r.service.config.RelayMemoryBytes {
		return true
	}
	return false
}

// acknowledge drops everything through seq from a stream's replay buffer.
func (r *workspaceRelay) acknowledge(stream *relayStream, through int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if through <= stream.ackedThrough {
		return
	}
	stream.ackedThrough = through
	kept := stream.buffer[:0]
	for _, buffered := range stream.buffer {
		if buffered.seq <= through {
			stream.bufferBytes -= buffered.bytes
			r.memory -= buffered.bytes
			continue
		}
		kept = append(kept, buffered)
	}
	stream.buffer = kept
	if stream.bufferBytes < 0 {
		stream.bufferBytes = 0
	}
	if r.memory < 0 {
		r.memory = 0
	}
}

// nextToRunner allocates the next runner-bound sequence for a stream.
func (r *workspaceRelay) nextToRunner(stream *relayStream) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.toRunner++
	return stream.toRunner
}

// nextToPerson allocates the next person-bound sequence for a stream.
func (r *workspaceRelay) nextToPerson(stream *relayStream) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.toPerson++
	return stream.toPerson
}

// detachedCount reports how many of a workspace's streams are parked waiting
// for a resume, for tests and diagnostics.
func (r *workspaceRelay) detachedCount(workspaceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return 0
	}
	return len(room.detached)
}

// memoryUsed reports the relay's buffered bytes, for tests and diagnostics.
func (r *workspaceRelay) memoryUsed() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory
}

// beginTerminalRecording starts recording one terminal stream. A nil state --
// which is what a hub with recording turned off produces -- records nothing and
// is not an error: workspaces.terminal.record is a setting, and a terminal with
// it off is still a terminal.
func (r *workspaceRelay) beginTerminalRecording(state *terminalStreamState) {
	if state == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[state.workspaceID]
	if room == nil {
		return
	}
	room.terminals[state.streamID] = state
}

// appendTerminalEvent adds one event to a stream's recording.
func (r *workspaceRelay) appendTerminalEvent(workspaceID, streamID, code, data string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return
	}
	if state := room.terminals[streamID]; state != nil {
		state.recording.Append(code, data, at)
	}
}

// resizeTerminalRecording records a window change and remembers the new size,
// so a recording written after a resize declares the window the session ended
// at rather than the one it opened with.
func (r *workspaceRelay) resizeTerminalRecording(workspaceID, streamID string, cols, rows int, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return
	}
	if state := room.terminals[streamID]; state != nil {
		state.recording.AppendResize(cols, rows, at)
		state.cols, state.rows = cols, rows
	}
}

// takeTerminalRecordings removes and returns the recordings of a room whose
// stream id satisfies match; a nil match takes every one. The caller must hold
// the relay lock, and must write the rows only after releasing it: a
// transaction under this lock would make every frame of every workspace on the
// hub wait on one disk.
// A terminal's `exit` is not its end, because the runner follows it with a
// `closed` that gives the stream's slot back, and that `closed` arrives here
// through closeStream. Finishing on `exit` would write the row one frame early
// and leave the close with nothing to take.
func (room *relayRoom) takeTerminalRecordings(match func(string) bool) []*terminalStreamState {
	if room == nil || len(room.terminals) == 0 {
		return nil
	}
	taken := []*terminalStreamState{}
	for streamID, state := range room.terminals {
		if match != nil && !match(streamID) {
			continue
		}
		delete(room.terminals, streamID)
		taken = append(taken, state)
	}
	return taken
}

// finishWorkspaceTerminalRecordings writes every open terminal recording of
// one workspace.
func (r *workspaceRelay) finishWorkspaceTerminalRecordings(ctx context.Context, workspaceID string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	recorded := room.takeTerminalRecordings(nil)
	r.mu.Unlock()
	r.service.finishTerminalRecordings(ctx, recorded)
}

// Connection plumbing.

// send queues a frame for the connection's writer. It never blocks: a
// connection that cannot keep up is closed, because blocking here would let
// one slow reader stall the goroutine serving someone else.
func (c *relayConnection) send(frame workspacesession.Frame) {
	c.enqueue(relayOutbound{frame: frame})
}

// sendFinal queues the last frame this connection will receive and the reason
// it then ends. The writer closes after the frame is on the wire, so a peer
// always learns why its socket went away.
func (c *relayConnection) sendFinal(frame workspacesession.Frame, reason string) {
	if !c.enqueue(relayOutbound{frame: frame, final: reason}) {
		c.close(reason)
	}
}

// enqueue hands one write to the writer and reports whether it was accepted.
func (c *relayConnection) enqueue(item relayOutbound) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- item:
		return true
	default:
		c.close(workspacesession.CodeOverflow)
		return false
	}
}

// revoke tells the person their authority changed and closes the connection.
func (c *relayConnection) revoke() {
	c.sendFinal(workspacesession.ErrorFrame("", "", workspacesession.CodeRevoked, "Access to this workspace has changed"),
		workspacesession.CodeRevoked)
}

// close ends the connection once, recording the first reason given.
func (c *relayConnection) close(reason string) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closeReason = reason
		c.mu.Unlock()
		close(c.done)
	})
}

// reason reports why the connection ended.
func (c *relayConnection) reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

// countIn and countOut accumulate the audit row's byte counters.
func (c *relayConnection) countIn(n int) {
	c.mu.Lock()
	c.bytesIn += int64(n)
	c.mu.Unlock()
}

func (c *relayConnection) countOut(n int) {
	c.mu.Lock()
	c.bytesOut += int64(n)
	c.mu.Unlock()
}

func (c *relayConnection) counters() (in, out int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytesIn, c.bytesOut
}

// writeLoop is the only writer of the socket. Every frame the hub sends passes
// through here, so a write that stalls affects this connection and nothing
// else.
func (c *relayConnection) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case item := <-c.out:
			encoded, err := json.Marshal(item.frame)
			if err != nil {
				c.close("encode_failed")
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
			err = c.socket.Write(writeCtx, websocket.MessageText, encoded)
			cancel()
			if err != nil {
				c.close("write_failed")
				return
			}
			c.countOut(len(encoded))
			if item.final != "" {
				c.close(item.final)
				return
			}
		}
	}
}
