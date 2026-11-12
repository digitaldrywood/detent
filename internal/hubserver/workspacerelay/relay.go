package workspacerelay

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const (
	WriteQueue   = 64
	WriteTimeout = 10 * time.Second
	ReadLimit    = workspacesession.MaxFrameBytes + (16 << 10)
)

type Deps struct {
	MemoryBytes              int64
	FailExecRuns             func(context.Context, []*ExecRun, string)
	FinishTerminalRecordings func(context.Context, []*TerminalStream)
	RunReason                func(context.Context, string, []*ExecRun) string
}

type Relay struct {
	deps        Deps
	mu          sync.Mutex
	rooms       map[string]*Room
	memory      int64
	stopped     bool
	attachments uint64
}

type Room struct {
	WorkspaceID  string
	Runner       *Connection
	people       map[string]*Connection
	Streams      map[string]*Stream
	detached     map[string]*Stream
	execRuns     map[string]*ExecRun
	terminals    map[string]*TerminalStream
	hubStreamSeq int
}

const HubStreamOwner = "relayhub"

type ExecRun struct {
	RunID          string
	ActionID       string
	Organization   tracker.OrganizationID
	Project        tracker.ProjectID
	WorkspaceID    string
	RunnerID       string
	LeaseID        string
	FencingToken   int64
	Claimant       string
	Revision       int64
	OutputSequence int64
	RunnerScope    hubauth.Input
	Output         []byte
	Truncated      bool
}

func (s *ExecRun) append(data []byte) {
	room := workspacesession.MaxExecOutputBytes - len(s.Output)
	if room <= 0 {
		s.Truncated = true
		return
	}
	if len(data) > room {
		data = data[:room]
		s.Truncated = true
	}
	s.Output = append(s.Output, data...)
}

type TerminalStream struct {
	ID             string
	WorkspaceID    string
	OrganizationID string
	ProjectID      string
	RelaySessionID string
	StreamID       string
	PrincipalID    string
	Subject        string
	Isolation      string
	SupportReason  string
	StartedAt      time.Time
	Cols           int
	Rows           int
	Recording      *workspacesession.Recording
}

type Stream struct {
	ID           string
	Channel      string
	ConnectionID string
	HubOwned     bool
	PrincipalID  string
	SessionID    string
	toRunner     int64
	toPerson     int64
	buffer       []BufferedFrame
	bufferBytes  int64
	ackedThrough int64
	detachedAt   time.Time
}

type Outbound struct {
	Frame workspacesession.Frame
	Final string
}

type BufferedFrame struct {
	seq   int64
	Frame workspacesession.Frame
	bytes int64
}

type Connection struct {
	ID            string
	WorkspaceID   string
	Scope         hubauth.Input
	Owner         workspacesession.Owner
	Socket        *websocket.Conn
	Runner        bool
	Actor         workspacesession.Actor
	PrincipalID   string
	SessionID     string
	SupportReason string
	HostedRole    string
	SessionHash   string
	Subject       string
	Out           chan Outbound
	Done          chan struct{}
	closeOnce     sync.Once
	closeReason   string
	streams       int
	streamSeq     int
	bytesIn       int64
	bytesOut      int64
	AuditID       string
	mu            sync.Mutex
}

func New(deps Deps) *Relay {
	return &Relay{deps: deps, rooms: map[string]*Room{}}
}

func (r *Relay) failExecRuns(ctx context.Context, states []*ExecRun, reason string) {
	if r.deps.FailExecRuns != nil {
		r.deps.FailExecRuns(ctx, states, reason)
	}
}

func (r *Relay) finishTerminalRecordings(ctx context.Context, states []*TerminalStream) {
	if r.deps.FinishTerminalRecordings != nil {
		r.deps.FinishTerminalRecordings(ctx, states)
	}
}

func (r *Relay) Stop(ctx context.Context) {
	r.mu.Lock()
	r.stopped = true
	connections := []*Connection{}
	for _, room := range r.rooms {
		if room.Runner != nil {
			connections = append(connections, room.Runner)
		}
		for _, person := range room.people {
			connections = append(connections, person)
		}
	}
	orphaned := []*ExecRun{}
	recorded := []*TerminalStream{}
	for _, room := range r.rooms {
		orphaned = append(orphaned, room.takeExecRuns(nil)...)
		recorded = append(recorded, room.takeTerminalRecordings(nil)...)
	}
	r.rooms = map[string]*Room{}
	r.memory = 0
	r.mu.Unlock()
	r.failExecRuns(ctx, orphaned, workspacesession.RunReasonStreamClosed)
	r.finishTerminalRecordings(ctx, recorded)
	for _, connection := range connections {
		connection.Close("server_shutdown")
	}
}

func (r *Relay) ensureRoom(workspaceID string) *Room {
	if room := r.rooms[workspaceID]; room != nil {
		return room
	}
	room := &Room{
		WorkspaceID: workspaceID,
		people:      map[string]*Connection{},
		Streams:     map[string]*Stream{},
		detached:    map[string]*Stream{},
		execRuns:    map[string]*ExecRun{},
		terminals:   map[string]*TerminalStream{},
	}
	r.rooms[workspaceID] = room
	return room
}

func (r *Relay) CloseWorkspace(ctx context.Context, workspaceID, reason string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	orphaned := room.takeExecRuns(nil)
	recorded := room.takeTerminalRecordings(nil)
	delete(r.rooms, workspaceID)
	connections := []*Connection{}
	if room.Runner != nil {
		connections = append(connections, room.Runner)
	}
	for _, person := range room.people {
		connections = append(connections, person)
	}
	for _, stream := range room.Streams {
		r.memory -= stream.bufferBytes
	}
	for _, stream := range room.detached {
		r.memory -= stream.bufferBytes
	}
	if r.memory < 0 {
		r.memory = 0
	}
	r.mu.Unlock()
	if len(orphaned) > 0 {
		r.failExecRuns(ctx, orphaned, r.deps.RunReason(ctx, workspaceID, orphaned))
	}
	r.finishTerminalRecordings(ctx, recorded)
	for _, connection := range connections {
		connection.SendFinal(workspacesession.ErrorFrame("", "", reason, "The workspace is no longer serving this connection"), reason)
	}
}

func (r *Relay) Sweep(ctx context.Context, now time.Time) {
	type expiry struct {
		room   *Room
		stream *Stream
	}
	r.mu.Lock()
	expired := []expiry{}
	orphaned := []*ExecRun{}
	recorded := []*TerminalStream{}
	for _, room := range r.rooms {
		for id, stream := range room.detached {
			if now.Sub(stream.detachedAt) < workspacesession.ResumeWindow {
				continue
			}
			delete(room.detached, id)
			r.memory -= stream.bufferBytes
			orphaned = append(orphaned, room.takeExecRuns(func(streamID string) bool { return streamID == id })...)
			recorded = append(recorded, room.takeTerminalRecordings(func(streamID string) bool { return streamID == id })...)
			expired = append(expired, expiry{room: room, stream: stream})
		}
	}
	if r.memory < 0 {
		r.memory = 0
	}
	runners := map[*Room]*Connection{}
	for _, item := range expired {
		runners[item.room] = item.room.Runner
	}
	r.mu.Unlock()
	r.failExecRuns(ctx, orphaned, workspacesession.RunReasonStreamClosed)
	r.finishTerminalRecordings(ctx, recorded)
	for _, item := range expired {
		if runner := runners[item.room]; runner != nil {
			runner.Send(workspacesession.Frame{Channel: item.stream.Channel, Stream: item.stream.ID, Type: workspacesession.TypeClose})
		}
	}
}

func (r *Relay) AttachPerson(connection *Connection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return errors.New("the relay is shutting down")
	}
	room := r.ensureRoom(connection.WorkspaceID)
	room.people[connection.ID] = connection
	r.attachments++
	return nil
}

func (r *Relay) DetachPerson(ctx context.Context, connection *Connection, now time.Time) {
	r.mu.Lock()
	room := r.rooms[connection.WorkspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	if room.people[connection.ID] == connection {
		delete(room.people, connection.ID)
	}
	runner := room.Runner
	closed := []*Stream{}
	for id, stream := range room.Streams {
		if stream.ConnectionID != connection.ID {
			continue
		}
		delete(room.Streams, id)
		if stream.Channel == workspacesession.ChannelExec {
			r.memory -= stream.bufferBytes
			if r.memory < 0 {
				r.memory = 0
			}
			closed = append(closed, stream)
			continue
		}
		stream.detachedAt = now
		room.detached[id] = stream
	}
	orphaned := room.takeExecRuns(func(id string) bool {
		owner, _, err := workspacesession.ParseStreamID(id)
		return err == nil && owner == connection.ID
	})
	if len(room.people) == 0 && room.Runner == nil && len(room.detached) == 0 {
		delete(r.rooms, connection.WorkspaceID)
	}
	r.mu.Unlock()
	r.failExecRuns(ctx, orphaned, workspacesession.RunReasonStreamClosed)
	if runner != nil {
		for _, stream := range closed {
			runner.Send(workspacesession.Frame{Channel: stream.Channel, Stream: stream.ID, Type: workspacesession.TypeClose})
		}
	}
}

func (r *Relay) AttachRunner(connection *Connection) (*Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, errors.New("the relay is shutting down")
	}
	room := r.ensureRoom(connection.WorkspaceID)
	previous := room.Runner
	room.Runner = connection
	r.attachments++
	return previous, nil
}

func (r *Relay) DetachRunner(ctx context.Context, connection *Connection) bool {
	r.mu.Lock()
	room := r.rooms[connection.WorkspaceID]
	if room == nil {
		r.mu.Unlock()
		return false
	}
	current := room.Runner == connection
	orphaned := []*ExecRun{}
	recorded := []*TerminalStream{}
	if current {
		room.Runner = nil
		orphaned = room.takeExecRuns(nil)
		recorded = room.takeTerminalRecordings(nil)
	}
	if len(room.people) == 0 && room.Runner == nil && len(room.detached) == 0 {
		delete(r.rooms, connection.WorkspaceID)
	}
	r.mu.Unlock()
	r.failExecRuns(ctx, orphaned, workspacesession.RunReasonStreamClosed)
	r.finishTerminalRecordings(ctx, recorded)
	return current
}

func (r *Relay) RunnerFor(workspaceID string) *Connection {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return nil
	}
	return room.Runner
}

func (r *Relay) OpenStream(connection *Connection, channel string) (*Stream, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.WorkspaceID]
	if room == nil {
		return nil, workspacesession.CodeWorkspaceClosed
	}
	if r.memory >= r.deps.MemoryBytes {
		return nil, workspacesession.CodeRelayBusy
	}
	if connection.streams >= workspacesession.MaxStreamsPerConnection {
		return nil, workspacesession.CodeStreamLimit
	}
	if len(room.Streams)+len(room.detached) >= workspacesession.MaxStreamsPerWorkspace {
		return nil, workspacesession.CodeStreamLimit
	}
	connection.streams++
	connection.streamSeq++
	stream := &Stream{
		ID: workspacesession.StreamID(connection.ID, connection.streamSeq), Channel: channel,
		ConnectionID: connection.ID, PrincipalID: connection.PrincipalID, SessionID: connection.SessionID,
	}
	room.Streams[stream.ID] = stream
	return stream, ""
}

func (r *Relay) OpenHubStream(workspaceID, channel string) (*Stream, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil || room.Runner == nil {
		return nil, workspacesession.CodeStaleExecution
	}
	if len(room.Streams)+len(room.detached) >= workspacesession.MaxStreamsPerWorkspace {
		return nil, workspacesession.CodeStreamLimit
	}
	room.hubStreamSeq++
	stream := &Stream{
		ID:      workspacesession.StreamID(HubStreamOwner, room.hubStreamSeq),
		Channel: channel, ConnectionID: HubStreamOwner, HubOwned: true,
	}
	room.Streams[stream.ID] = stream
	return stream, ""
}

func (r *Relay) ChannelStream(connection *Connection, channel string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.WorkspaceID]
	if room == nil {
		return nil
	}
	var found *Stream
	lowest := 0
	for id, stream := range room.Streams {
		if stream.ConnectionID != connection.ID || stream.Channel != channel {
			continue
		}
		owner, index, err := workspacesession.ParseStreamID(id)
		if err != nil || owner != connection.ID {
			continue
		}
		if found == nil || index < lowest {
			found, lowest = stream, index
		}
	}
	return found
}

func (r *Relay) LookupStream(connection *Connection, id string) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.WorkspaceID]
	if room == nil {
		return nil
	}
	stream := room.Streams[id]
	if stream == nil || stream.ConnectionID != connection.ID {
		return nil
	}
	return stream
}

func (r *Relay) RunnerStream(runner *Connection, id string) (*Stream, *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[runner.WorkspaceID]
	if room == nil || room.Runner != runner {
		return nil, nil
	}
	if stream := room.Streams[id]; stream != nil {
		return stream, room.people[stream.ConnectionID]
	}
	if stream := room.detached[id]; stream != nil {
		return stream, nil
	}
	return nil, nil
}

func (r *Relay) CloseStream(ctx context.Context, workspaceID, id string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	if room == nil {
		r.mu.Unlock()
		return
	}
	for _, set := range []map[string]*Stream{room.Streams, room.detached} {
		stream := set[id]
		if stream == nil {
			continue
		}
		delete(set, id)
		if owner := room.people[stream.ConnectionID]; owner != nil && owner.streams > 0 {
			owner.streams--
		}
		r.memory -= stream.bufferBytes
		if r.memory < 0 {
			r.memory = 0
		}
	}
	orphaned := room.takeExecRuns(func(streamID string) bool { return streamID == id })
	recorded := room.takeTerminalRecordings(func(streamID string) bool { return streamID == id })
	r.mu.Unlock()
	r.failExecRuns(ctx, orphaned, workspacesession.RunReasonStreamClosed)
	r.finishTerminalRecordings(ctx, recorded)
}

func (r *Relay) ResumeStream(connection *Connection, id string, lastSeq int64, now time.Time) (*Stream, []workspacesession.Frame, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[connection.WorkspaceID]
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
	if stream.PrincipalID != connection.PrincipalID || stream.SessionID != connection.SessionID {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if lastSeq < 0 || lastSeq > stream.toPerson {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	replay := []workspacesession.Frame{}
	for _, buffered := range stream.buffer {
		if buffered.seq > lastSeq {
			replay = append(replay, buffered.Frame)
		}
	}
	if len(replay) == 0 && lastSeq < stream.toPerson {
		return nil, nil, workspacesession.CodeResumeFailed
	}
	if connection.streams >= workspacesession.MaxStreamsPerConnection {
		return nil, nil, workspacesession.CodeStreamLimit
	}
	delete(room.detached, id)
	stream.ConnectionID = connection.ID
	stream.detachedAt = time.Time{}
	r.releaseThrough(stream, lastSeq)
	room.Streams[id] = stream
	connection.streams++
	return stream, replay, ""
}

func (r *Relay) BufferForPerson(workspaceID string, stream *Stream, frame workspacesession.Frame, seq int64) bool {
	size := int64(len(frame.Payload)) + 64
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.buffer = append(stream.buffer, BufferedFrame{seq: seq, Frame: frame, bytes: size})
	stream.bufferBytes += size
	r.memory += size
	if stream.bufferBytes > workspacesession.StreamBufferBytes {
		return true
	}
	if r.memory > r.deps.MemoryBytes {
		return true
	}
	return false
}

func (r *Relay) Acknowledge(stream *Stream, through int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseThrough(stream, through)
}

func (r *Relay) releaseThrough(stream *Stream, through int64) {
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

func (r *Relay) NextToRunner(stream *Stream) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.toRunner++
	return stream.toRunner
}

func (r *Relay) NextToPerson(stream *Stream) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stream.toPerson++
	return stream.toPerson
}

func (r *Relay) DetachedCount(workspaceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return 0
	}
	return len(room.detached)
}

func (r *Relay) Attachments() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attachments
}

func (r *Relay) StreamCount(workspaceID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return 0
	}
	return len(room.Streams) + len(room.detached)
}

func (r *Relay) Room(workspaceID string) *Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rooms[workspaceID]
}

func (r *Relay) MemoryUsed() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.memory
}

func (room *Room) takeExecRuns(match func(string) bool) []*ExecRun {
	if room == nil || len(room.execRuns) == 0 {
		return nil
	}
	taken := []*ExecRun{}
	for id, state := range room.execRuns {
		if match != nil && !match(id) {
			continue
		}
		delete(room.execRuns, id)
		taken = append(taken, state)
	}
	return taken
}

func (r *Relay) BeginTerminalRecording(state *TerminalStream) {
	if state == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[state.WorkspaceID]
	if room == nil {
		return
	}
	room.terminals[state.StreamID] = state
}

func (r *Relay) AppendTerminalEvent(workspaceID, streamID, code, data string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return
	}
	if state := room.terminals[streamID]; state != nil {
		state.Recording.Append(code, data, at)
	}
}

func (r *Relay) ResizeTerminalRecording(workspaceID, streamID string, cols, rows int, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return
	}
	if state := room.terminals[streamID]; state != nil {
		state.Recording.AppendResize(cols, rows, at)
		state.Cols, state.Rows = cols, rows
	}
}

func (room *Room) takeTerminalRecordings(match func(string) bool) []*TerminalStream {
	if room == nil || len(room.terminals) == 0 {
		return nil
	}
	taken := []*TerminalStream{}
	for streamID, state := range room.terminals {
		if match != nil && !match(streamID) {
			continue
		}
		delete(room.terminals, streamID)
		taken = append(taken, state)
	}
	return taken
}

func (r *Relay) BeginExecRun(workspaceID, streamID string, state *ExecRun, runner *Connection) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil || room.Runner == nil || (runner != nil && room.Runner != runner) || room.Streams[streamID] == nil {
		return false
	}
	room.execRuns[streamID] = state
	return true
}

func (r *Relay) AppendExecOutput(workspaceID, streamID string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return
	}
	if state := room.execRuns[streamID]; state != nil {
		state.append(data)
	}
}

func (r *Relay) TakeExecRun(workspaceID, streamID string) (*ExecRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.rooms[workspaceID]
	if room == nil {
		return nil, false
	}
	state := room.execRuns[streamID]
	if state == nil {
		return nil, false
	}
	delete(room.execRuns, streamID)
	return state, true
}

func (r *Relay) FailWorkspaceExecRuns(ctx context.Context, workspaceID, reason string) {
	r.mu.Lock()
	room := r.rooms[workspaceID]
	taken := room.takeExecRuns(nil)
	recorded := room.takeTerminalRecordings(nil)
	r.mu.Unlock()
	r.failExecRuns(ctx, taken, reason)
	r.finishTerminalRecordings(ctx, recorded)
}

func (c *Connection) Send(frame workspacesession.Frame) {
	c.enqueue(Outbound{Frame: frame})
}

func (c *Connection) SendFinal(frame workspacesession.Frame, reason string) {
	if !c.enqueue(Outbound{Frame: frame, Final: reason}) {
		c.Close(reason)
	}
}

func (c *Connection) enqueue(item Outbound) bool {
	select {
	case <-c.Done:
		return false
	default:
	}
	select {
	case c.Out <- item:
		return true
	default:
		c.Close(workspacesession.CodeOverflow)
		return false
	}
}

func (c *Connection) Revoke() {
	c.SendFinal(workspacesession.ErrorFrame("", "", workspacesession.CodeRevoked, "Access to this workspace has changed"),
		workspacesession.CodeRevoked)
}

func (c *Connection) Close(reason string) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closeReason = reason
		c.mu.Unlock()
		close(c.Done)
	})
}

func (c *Connection) Reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

func (c *Connection) CountIn(n int) {
	c.mu.Lock()
	c.bytesIn += int64(n)
	c.mu.Unlock()
}

func (c *Connection) CountOut(n int) {
	c.mu.Lock()
	c.bytesOut += int64(n)
	c.mu.Unlock()
}

func (c *Connection) Counters() (in, out int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytesIn, c.bytesOut
}

func (c *Connection) WriteLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done:
			return
		case item := <-c.Out:
			encoded, err := json.Marshal(item.Frame)
			if err != nil {
				c.Close("encode_failed")
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, WriteTimeout)
			err = c.Socket.Write(writeCtx, websocket.MessageText, encoded)
			cancel()
			if err != nil {
				c.Close("write_failed")
				return
			}
			c.CountOut(len(encoded))
			if item.Final != "" {
				c.Close(item.Final)
				return
			}
		}
	}
}
