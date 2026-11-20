package sessiontimeout

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	DefaultTurn = time.Hour
	DefaultIdle = 5 * time.Minute
	maxEvents   = 64 * 1024
)

var (
	ErrIdle         = errors.New("provider session idle")
	errTurnDeadline = errors.New("provider turn limit elapsed")
)

type Kind string

const (
	Idle            Kind = "session_idle_timeout"
	CommandDeadline Kind = "command_deadline"
	TurnDeadline    Kind = "turn_deadline"
)

type Error struct {
	Kind  Kind          `json:"kind"`
	Limit time.Duration `json:"limit_ns"`
}

func (e *Error) Valid() bool {
	return e != nil && e.Limit > 0 && (e.Kind == Idle || e.Kind == CommandDeadline || e.Kind == TurnDeadline)
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s after %s", e.Kind, e.Limit)
}

func (e *Error) Unwrap() error {
	if e.Kind == Idle {
		return ErrIdle
	}
	return context.DeadlineExceeded
}

type sessionKey struct{}

type Session struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	stop      context.CancelFunc
	idle      time.Duration
	limit     time.Duration
	mu        sync.Mutex
	timer     *time.Timer
	expires   time.Time
	seen      map[[sha256.Size]byte]struct{}
	seenOrder [][sha256.Size]byte
	oldest    int
	commands  map[string]struct{}
}

func Start(parent context.Context, idle, limit time.Duration, timeout func(context.Context, time.Duration, error) (context.Context, context.CancelFunc)) *Session {
	if limit <= 0 {
		limit = DefaultTurn
	}
	if timeout == nil {
		timeout = context.WithTimeoutCause
	}
	deadline := parent
	stop := func() {}
	if inherited, ok := parent.Deadline(); !ok || time.Until(inherited) > limit {
		deadline, stop = timeout(parent, limit, errTurnDeadline)
	}
	ctx, cancel := context.WithCancelCause(deadline)
	s := &Session{cancel: cancel, stop: stop, idle: idle, limit: limit}
	s.ctx = context.WithValue(ctx, sessionKey{}, s)
	s.Activity()
	return s
}

func (s *Session) Context() context.Context { return s.ctx }

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel(context.Canceled)
	if s.timer != nil {
		s.timer.Stop()
	}
	s.stop()
}

func (s *Session) Result(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var observed *Error
	if errors.As(err, &observed) {
		return err
	}
	cause := context.Cause(s.ctx)
	kind := TurnDeadline
	if len(s.commands) > 0 {
		kind = CommandDeadline
	}
	if errors.Is(cause, errTurnDeadline) {
		cause = &Error{Kind: kind, Limit: s.limit}
	}
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return errors.Join(cause, err)
}

func (s *Session) Activity() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activity()
}

func (s *Session) Event(identity string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.newEvent(identity) {
		return false
	}
	s.activity()
	return true
}

func ObserveOwnedCommand(ctx context.Context, id string) func() {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	deadline, bounded := ctx.Deadline()
	if s == nil || !bounded || ctx.Err() != nil {
		return func() {}
	}
	id = "host/" + id
	s.StartCommand(id)
	limit := max(time.Nanosecond, time.Until(deadline))
	onDeadline := func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.cancel(&Error{Kind: CommandDeadline, Limit: limit})
		}
	}
	stop := context.AfterFunc(ctx, onDeadline)
	return func() {
		stop()
		onDeadline()
		s.EndCommand(id)
	}
}

func (s *Session) StartCommand(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.commands[id]; running || s.ctx.Err() != nil {
		return false
	}
	if _, finished := s.seen[sha256.Sum256([]byte("command/end/"+id))]; finished {
		return false
	}
	if s.commands == nil {
		s.commands = make(map[string]struct{})
	}
	s.commands[id] = struct{}{}
	if s.timer != nil {
		s.timer.Stop()
	}
	return true
}

func (s *Session) EndCommand(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	_, running := s.commands[id]
	if !s.newEvent("command/end/"+id) && !running {
		return false
	}
	delete(s.commands, id)
	s.activity()
	return true
}

func (s *Session) newEvent(identity string) bool {
	if identity == "" || s.ctx.Err() != nil {
		return false
	}
	if s.seen == nil {
		s.seen = make(map[[sha256.Size]byte]struct{})
	}
	key := sha256.Sum256([]byte(identity))
	if _, exists := s.seen[key]; exists {
		return false
	}
	if len(s.seenOrder) == maxEvents {
		delete(s.seen, s.seenOrder[s.oldest])
		s.seenOrder[s.oldest] = key
		s.oldest = (s.oldest + 1) % maxEvents
	} else {
		s.seenOrder = append(s.seenOrder, key)
	}
	s.seen[key] = struct{}{}
	return true
}

func (s *Session) activity() bool {
	if s.ctx.Err() != nil {
		return false
	}
	if s.idle <= 0 || len(s.commands) > 0 {
		return true
	}
	s.expires = time.Now().Add(s.idle)
	if s.timer == nil {
		s.timer = time.AfterFunc(s.idle, s.expireIdle)
	} else {
		s.timer.Reset(s.idle)
	}
	return true
}

func (s *Session) expireIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() == nil && len(s.commands) == 0 && !time.Now().Before(s.expires) {
		s.cancel(&Error{Kind: Idle, Limit: s.idle})
	}
}
