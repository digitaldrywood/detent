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
	Kind  Kind
	Limit time.Duration
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

type Session struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	stop     context.CancelFunc
	idle     time.Duration
	limit    time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	expires  time.Time
	seen     map[[sha256.Size]byte]struct{}
	commands map[string]struct{}
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
	s := &Session{ctx: ctx, cancel: cancel, stop: stop, idle: idle, limit: limit}
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

func (s *Session) Activity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity()
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

func (s *Session) StartCommand(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, finished := s.seen[sha256.Sum256([]byte("command/end/"+id))]; finished || !s.newEvent("command/start/"+id) {
		return
	}
	if s.commands == nil {
		s.commands = make(map[string]struct{})
	}
	s.commands[id] = struct{}{}
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (s *Session) EndCommand(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.newEvent("command/end/" + id) {
		return
	}
	delete(s.commands, id)
	s.activity()
}

func (s *Session) newEvent(identity string) bool {
	if identity == "" || s.ctx.Err() != nil || len(s.seen) >= maxEvents {
		return false
	}
	if s.seen == nil {
		s.seen = make(map[[sha256.Size]byte]struct{})
	}
	key := sha256.Sum256([]byte(identity))
	if _, exists := s.seen[key]; exists {
		return false
	}
	s.seen[key] = struct{}{}
	return true
}

func (s *Session) activity() {
	if s.ctx.Err() != nil || s.idle <= 0 || len(s.commands) > 0 {
		return
	}
	s.expires = time.Now().Add(s.idle)
	if s.timer == nil {
		s.timer = time.AfterFunc(s.idle, s.expireIdle)
	} else {
		s.timer.Reset(s.idle)
	}
}

func (s *Session) expireIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() == nil && len(s.commands) == 0 && !time.Now().Before(s.expires) {
		s.cancel(&Error{Kind: Idle, Limit: s.idle})
	}
}
