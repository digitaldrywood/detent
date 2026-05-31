package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Transport interface {
	Send(context.Context, Message) error
	Receive(context.Context) (Message, error)
	Close(context.Context) error
}

type TransportFactory interface {
	NewTransport(context.Context) (Transport, error)
}

type CommandFactory func(context.Context) *exec.Cmd

type LocalTransportFactory struct {
	newCommand CommandFactory
}

type localTransport struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	codec     *Codec
	received  chan transportResult
	readDone  chan struct{}
	done      chan struct{}
	sendLock  chan struct{}
	waitErr   error
	waitMu    sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

type transportResult struct {
	msg Message
	err error
}

func NewLocalTransportFactory(newCommand CommandFactory) (*LocalTransportFactory, error) {
	if newCommand == nil {
		return nil, errors.New("command factory is nil")
	}

	return &LocalTransportFactory{newCommand: newCommand}, nil
}

func (f *LocalTransportFactory) NewTransport(ctx context.Context) (Transport, error) {
	ctx = contextOrBackground(ctx)

	cmd := f.newCommand(ctx)
	if cmd == nil {
		return nil, errors.New("command factory returned nil command")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start command: %w", err)
	}

	transport := &localTransport{
		cmd:      cmd,
		stdin:    stdin,
		codec:    NewCodec(stdout, stdin),
		received: make(chan transportResult, 64),
		readDone: make(chan struct{}),
		done:     make(chan struct{}),
		sendLock: make(chan struct{}, 1),
	}
	transport.sendLock <- struct{}{}

	go transport.readLoop()
	go transport.wait()

	return transport, nil
}

func (t *localTransport) Send(ctx context.Context, msg Message) error {
	ctx = contextOrBackground(ctx)

	if err := t.acquireSend(ctx); err != nil {
		return err
	}
	defer t.releaseSend()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- t.codec.WriteMessage(msg)
	}()

	select {
	case err := <-writeDone:
		return err
	case <-ctx.Done():
		select {
		case err := <-writeDone:
			return err
		default:
		}

		closeErr := t.closeStdin()
		<-writeDone
		if closeErr != nil {
			return fmt.Errorf("%w: close stdin: %v", ctx.Err(), closeErr)
		}
		return ctx.Err()
	}
}

func (t *localTransport) Receive(ctx context.Context) (Message, error) {
	ctx = contextOrBackground(ctx)

	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case result, ok := <-t.received:
		if !ok {
			return Message{}, io.EOF
		}
		return result.msg, result.err
	}
}

func (t *localTransport) Close(ctx context.Context) error {
	ctx = contextOrBackground(ctx)

	closeErr := t.closeStdin()

	select {
	case <-t.done:
		if closeErr != nil {
			return fmt.Errorf("close stdin: %w", closeErr)
		}
		return t.waitError()
	case <-ctx.Done():
		var killErr error
		if t.cmd.Process != nil {
			killErr = t.cmd.Process.Kill()
		}

		select {
		case <-t.done:
		case <-time.After(time.Second):
		}

		if killErr != nil {
			return fmt.Errorf("%w: kill process: %v", ctx.Err(), killErr)
		}
		return ctx.Err()
	}
}

func (t *localTransport) acquireSend(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return t.closedError()
	case <-t.sendLock:
		select {
		case <-t.done:
			t.releaseSend()
			return t.closedError()
		default:
			return nil
		}
	}
}

func (t *localTransport) releaseSend() {
	t.sendLock <- struct{}{}
}

func (t *localTransport) closeStdin() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.stdin.Close()
		if errors.Is(t.closeErr, os.ErrClosed) {
			t.closeErr = nil
		}
	})
	return t.closeErr
}

func (t *localTransport) closedError() error {
	if err := t.waitError(); err != nil {
		return fmt.Errorf("transport closed: %w", err)
	}
	return errors.New("transport closed")
}

func (t *localTransport) readLoop() {
	defer close(t.readDone)
	defer close(t.received)

	for {
		msg, err := t.codec.ReadMessage()
		if err != nil {
			t.received <- transportResult{err: err}
			return
		}

		t.received <- transportResult{msg: msg}
	}
}

func (t *localTransport) wait() {
	<-t.readDone
	err := t.cmd.Wait()
	t.waitMu.Lock()
	t.waitErr = err
	t.waitMu.Unlock()
	close(t.done)
}

func (t *localTransport) waitError() error {
	t.waitMu.Lock()
	defer t.waitMu.Unlock()
	return t.waitErr
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
