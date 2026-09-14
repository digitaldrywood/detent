package workspacerunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

// The terminal channel on the runner (decisions section 18.3).
//
// It is the first channel on this session that is neither request/response
// (files, git) nor one-shot streaming (exec). A PTY outlives the frame that
// asked for it, outlives the frame that last fed it, and -- for the resume
// window of section 18.2 -- outlives the socket it was speaking over. Every
// piece of bookkeeping here exists because of one of those three.

// terminalStream is one PTY held for one relay stream.
//
// A stream is never shared between connections (section 18.2), so one stream is
// exactly one PTY and the stream id is the whole key.
type terminalStream struct {
	channel  string
	terminal *workspaceterminal.Terminal

	mu sync.Mutex
	// reaper kills a PTY whose socket has been gone for the resume window. It
	// is nil while a socket is attached, which is the ordinary case: a person
	// disconnecting does not drop the runner's socket, and the hub's own sweep
	// sends this runner a close for a stream nobody resumed. This timer is for
	// the other half -- the runner's own socket dropping -- where no close can
	// arrive because there is nothing to carry it.
	reaper *time.Timer
}

// detach starts the resume window for one PTY.
func (t *terminalStream) detach(after time.Duration, kill func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reaper != nil {
		return
	}
	t.reaper = time.AfterFunc(after, kill)
}

// attach cancels a resume window that has not run out.
func (t *terminalStream) attach() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reaper == nil {
		return
	}
	t.reaper.Stop()
	t.reaper = nil
}

// terminalService reports the service that opens PTYs for this workspace, or
// nil when this runner serves no terminal: the platform has none, the build was
// asked not to, or the level the organization requires is one this runner
// cannot provide.
func (s *Session) terminalService() *workspaceterminal.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *Session) setTerminalService(service *workspaceterminal.Service) {
	s.mu.Lock()
	s.terminal = service
	s.mu.Unlock()
}

// handleTerminal answers one frame on the terminal channel.
//
// Unlike the files and git channels there is no single answer to return: an
// open is answered with `opened` and then with output frames for as long as the
// shell lives, an input is answered with whatever the shell writes back, and a
// resize is answered with nothing at all. So each case writes its own answers
// and this function's job is to refuse what should not happen and to get out of
// the read loop's way.
func (s *Session) handleTerminal(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	if !workspacesession.ValidTerminalRequest(frame.Type) {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnknownFrame, "The terminal channel does not define "+frame.Type))
		return
	}
	// Read-only is checked before the service is, and the order is the whole
	// difference between two sentences a reader has to tell apart. A read-only
	// workspace is one whose attempt is still running (section 18.1), and this
	// session opened no terminal service for exactly that reason -- so a
	// service-first check would answer "this runner does not serve terminals",
	// which is false and sends the reader looking at their runner instead of at
	// the attempt they have to wait for.
	//
	// Every terminal frame is checked, not just the open: a terminal is a write
	// in its entirety, and there is no read-only half of typing into a shell.
	if refusal, ok := s.refuseWrite(); !ok {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeReadOnly, refusal))
		return
	}
	service := s.terminalService()
	if service == nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnsupported, terminalRefusalMessage(workspacesession.CodeUnsupported)))
		return
	}
	// The lease is validated immediately before every frame is acted on, and
	// for a terminal that means immediately before every keystroke reaches the
	// PTY. Section 18.2 requires it and section 18.3 is why it matters: a frame
	// that arrives after lease loss would be typing into a worktree this
	// generation no longer owns.
	if !s.leaseValid() {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeStaleExecution, refusalMessage(workspacesession.CodeStaleExecution)))
		return
	}
	// A frame on a stream is proof its reader is back, so the resume window
	// this runner opened when its socket dropped is closed again.
	s.attachTerminal(frame.Stream)

	switch frame.Type {
	case workspacesession.TypeTerminalOpen:
		s.openTerminal(ctx, socket, service, frame)
	case workspacesession.TypeTerminalInput:
		s.writeTerminal(ctx, socket, frame)
	case workspacesession.TypeTerminalResize:
		s.resizeTerminal(ctx, socket, frame)
	}
}

// openTerminal allocates a PTY for one stream.
func (s *Session) openTerminal(
	ctx context.Context,
	socket *websocket.Conn,
	service *workspaceterminal.Service,
	frame workspacesession.Frame,
) {
	var request workspacesession.TerminalOpen
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The open payload could not be read"))
			return
		}
	}
	if _, held := s.lookupTerminal(frame.Stream); held {
		// One stream is one PTY. A second open on a stream already holding one
		// would leave the person with two shells writing into one sequence
		// space and one exit frame to explain both.
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeInvalidFrame, "This stream already has a terminal"))
		return
	}
	if s.terminalCount() >= maxConcurrentTerminals {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeStreamLimit, terminalRefusalMessage(workspacesession.CodeStreamLimit)))
		return
	}

	stream := frame.Stream
	channel := frame.Channel
	// Output is written through the session rather than to this socket: a PTY
	// outlives the socket it was opened on, and a span written to a socket that
	// has since been replaced would be a span nobody receives.
	//
	// The context is the session's --- handle is called from the relay read
	// loop, which runs on it --- rather than one of this frame's, because the
	// spans this closure writes go on arriving long after the open that
	// installed it has returned.
	sessionCtx := ctx
	emit := func(span workspacesession.TerminalOutput) error {
		payload, err := workspacesession.Encode(span)
		if err != nil {
			return err
		}
		return s.push(sessionCtx, workspacesession.Frame{
			Channel: channel, Stream: stream, Type: workspacesession.TypeTerminalOutput, Payload: payload,
		})
	}

	terminal, err := service.Open(ctx, request, emit)
	if err != nil {
		code := workspaceterminal.ErrorCode(err)
		if code == "" {
			code = workspacesession.CodeForbidden
		}
		s.logger.Info("workspace.terminal_refused", "stream", stream, "code", code, "error", err)
		s.answer(ctx, socket, workspacesession.ErrorFrame(channel, stream, code, terminalRefusalMessage(code)))
		return
	}
	s.holdTerminal(stream, &terminalStream{channel: channel, terminal: terminal})

	cols, rows := terminal.Size()
	opened, err := workspacesession.Encode(workspacesession.TerminalOpened{
		PID: terminal.PID(), Isolation: terminal.Isolation(), Cols: cols, Rows: rows,
	})
	if err != nil {
		s.logger.Warn("workspace.terminal_open_not_encoded", "stream", stream, "error", err)
		s.closeTerminal(stream)
		return
	}
	s.answer(ctx, socket, workspacesession.Frame{
		Channel: channel, Stream: stream, Type: workspacesession.TypeTerminalOpened, Payload: opened,
	})

	// The exit and the close are written by a watcher of the PTY's own, because
	// nothing else will: a shell a person typed `exit` into ends with no frame
	// having arrived to notice. It runs on the session's context for the reason
	// emit does: it outlives this frame by design.
	go s.watchTerminalExit(sessionCtx, stream)
}

// watchTerminalExit reports a shell's end and gives the stream back.
func (s *Session) watchTerminalExit(ctx context.Context, stream string) {
	held, ok := s.lookupTerminal(stream)
	if !ok {
		return
	}
	result := held.terminal.Wait()
	s.releaseTerminal(stream)
	s.logger.Info("workspace.terminal_exited", "stream", stream, "code", result.ExitCode, "signal", result.Signal)

	if payload, err := workspacesession.Encode(workspacesession.TerminalExit{
		Code: result.ExitCode, Signal: result.Signal,
	}); err == nil {
		if err := s.push(ctx, workspacesession.Frame{
			Channel: held.channel, Stream: stream, Type: workspacesession.TypeTerminalExit, Payload: payload,
		}); err != nil {
			s.logger.Debug("workspace.terminal_exit_not_sent", "stream", stream, "error", err)
		}
	} else {
		s.logger.Warn("workspace.terminal_exit_not_encoded", "stream", stream, "error", err)
	}
	// The hub releases a stream's slot on close or closed and never on exit. A
	// runner that stopped at the exit frame would leak one slot per terminal
	// until the workspace hit its stream cap and started refusing the next
	// open, which is exactly the leak section 18.12 writes out for exec.
	if err := s.push(ctx, workspacesession.Frame{
		Channel: held.channel, Stream: stream, Type: workspacesession.TypeClosed,
	}); err != nil {
		s.logger.Debug("workspace.terminal_closed_not_sent", "stream", stream, "error", err)
	}
}

// writeTerminal feeds one input frame to its PTY.
func (s *Session) writeTerminal(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	var request workspacesession.TerminalInput
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The input payload could not be read"))
			return
		}
	}
	if err := workspacesession.ValidateTerminalInput(request); err != nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeInvalidFrame, err.Error()))
		return
	}
	data := []byte(request.Data)
	if request.Encoding == workspacesession.EncodingBase64 {
		decoded, err := base64.StdEncoding.DecodeString(request.Data)
		if err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The input was marked base64 and did not decode"))
			return
		}
		data = decoded
	}
	held, ok := s.lookupTerminal(frame.Stream)
	if !ok {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeNotFound, terminalRefusalMessage(workspacesession.CodeNotFound)))
		return
	}
	if err := held.terminal.Write(data); err != nil {
		code := workspaceterminal.ErrorCode(err)
		if code == "" {
			code = workspacesession.CodeForbidden
		}
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code,
			terminalRefusalMessage(code)))
	}
}

// resizeTerminal applies a new window to one PTY.
func (s *Session) resizeTerminal(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	var request workspacesession.TerminalResize
	if err := json.Unmarshal(frame.Payload, &request); err != nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeInvalidFrame, "The resize payload could not be read"))
		return
	}
	size, err := workspacesession.ValidateTerminalResize(request)
	if err != nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeInvalidFrame, err.Error()))
		return
	}
	held, ok := s.lookupTerminal(frame.Stream)
	if !ok {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeNotFound, terminalRefusalMessage(workspacesession.CodeNotFound)))
		return
	}
	if err := held.terminal.Resize(size.Cols, size.Rows); err != nil {
		code := workspaceterminal.ErrorCode(err)
		if code == "" {
			code = workspacesession.CodeForbidden
		}
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code,
			terminalRefusalMessage(code)))
	}
	// A successful resize is answered with nothing. The window is not a fact a
	// client asked to be told; it is a fact it supplied, and a program in the
	// shell learns about it from SIGWINCH rather than from the relay.
}

// maxConcurrentTerminals bounds how many PTYs one workspace holds at once. It
// is the relay's own per-workspace stream cap (section 18.2), so a runner never
// refuses an open the hub would have allowed, and never holds more shells than
// there are streams to carry them.
const maxConcurrentTerminals = workspacesession.MaxStreamsPerWorkspace

// lookupTerminal reports the PTY on one stream.
func (s *Session) lookupTerminal(stream string) (*terminalStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.terminals[stream]
	if !ok || held == nil {
		// A stream that was never held, or one whose entry was cleared, is
		// the same answer to every caller: nothing to write to, nothing to
		// wait on. Saying so here is what lets each of them read the pointer
		// without its own guard.
		return nil, false
	}
	return held, true
}

// holdTerminal registers a PTY against its stream.
func (s *Session) holdTerminal(stream string, held *terminalStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminals == nil {
		s.terminals = map[string]*terminalStream{}
	}
	s.terminals[stream] = held
}

// releaseTerminal forgets a PTY that has ended, without signalling it.
func (s *Session) releaseTerminal(stream string) {
	s.mu.Lock()
	held := s.terminals[stream]
	delete(s.terminals, stream)
	s.mu.Unlock()
	if held != nil {
		held.attach()
	}
}

// terminalCount reports how many PTYs this session is holding.
func (s *Session) terminalCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.terminals)
}

// closeTerminal ends the PTY on one stream: SIGHUP, then SIGKILL after the
// grace, then the PTY itself. It is what a person's close frame reaches, and
// what the hub's own sweep reaches when nobody resumed the stream.
func (s *Session) closeTerminal(stream string) {
	held, ok := s.lookupTerminal(stream)
	if !ok {
		return
	}
	s.logger.Debug("workspace.terminal_closed", "stream", stream)
	held.attach()
	held.terminal.Close()
}

// closeTerminals ends every PTY this session holds. It is the workspace ending,
// the lease being lost, or the process shutting down: in all three the worktree
// is about to stop being this generation's, and a shell left running in it is
// the loudest way to keep touching it.
func (s *Session) closeTerminals(reason string) {
	s.mu.Lock()
	held := make([]*terminalStream, 0, len(s.terminals))
	for _, terminal := range s.terminals {
		held = append(held, terminal)
	}
	s.mu.Unlock()
	if len(held) == 0 {
		return
	}
	s.logger.Info("workspace.terminals_closed", "terminals", len(held), "reason", reason)
	for _, terminal := range held {
		terminal.attach()
		terminal.terminal.Close()
	}
}

// detachTerminals starts the resume window for every PTY this session holds.
//
// Section 18.2 gives a dropped connection 60 seconds to come back and says the
// runner keeps a disconnected PTY alive exactly that long. Two different
// disconnections can reach a terminal, and only one of them arrives as a frame.
// A person closing their tab leaves this runner's socket untouched: the hub
// parks their stream, and its sweep sends this runner a close once the window
// has passed. The runner's own socket dropping carries nothing at all -- there
// is no channel left to carry a close on -- so this is the half the runner has
// to time for itself. A redial inside the window cancels it, because the hub
// still holds the streams and the shells are still the ones behind them.
func (s *Session) detachTerminals() {
	s.mu.Lock()
	held := make(map[string]*terminalStream, len(s.terminals))
	for stream, terminal := range s.terminals {
		held[stream] = terminal
	}
	s.mu.Unlock()
	for stream, terminal := range held {
		terminal.detach(workspacesession.ResumeWindow, func() {
			s.logger.Info("workspace.terminal_resume_window_expired", "stream", stream)
			s.closeTerminal(stream)
		})
	}
}

// attachTerminal cancels one stream's resume window.
func (s *Session) attachTerminal(stream string) {
	if held, ok := s.lookupTerminal(stream); ok {
		held.attach()
	}
}

// attachTerminals cancels every resume window, which is what a redial means:
// the hub still holds these streams, and the shells behind them are still the
// ones their readers were looking at.
func (s *Session) attachTerminals() {
	s.mu.Lock()
	held := make([]*terminalStream, 0, len(s.terminals))
	for _, terminal := range s.terminals {
		held = append(held, terminal)
	}
	s.mu.Unlock()
	for _, terminal := range held {
		terminal.attach()
	}
}

// terminalRefusalMessage is the sentence that goes with a terminal refusal. It
// says what this runner would not do without repeating anything a person typed,
// which they may be reading in a shared panel.
func terminalRefusalMessage(code string) string {
	switch code {
	case workspacesession.CodeUnsupported:
		return "This runner does not serve terminals"
	case workspacesession.CodeNotFound:
		return "That terminal has ended"
	case workspacesession.CodeStreamLimit:
		return "This workspace already holds as many terminals as it may"
	case workspacesession.CodeInvalidFrame:
		return "The terminal frame could not be read"
	case workspacesession.CodeReadOnly:
		return "This workspace is read-only, so it cannot be typed into"
	case workspacesession.CodeStaleExecution:
		return "The workspace lease is no longer held by this runner"
	default:
		return "The terminal request was refused"
	}
}
