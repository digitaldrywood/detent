package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

type validationEvent struct {
	RunID                string    `json:"run_id"`
	Host                 string    `json:"host"`
	Identifier           string    `json:"identifier,omitempty"`
	Workspace            string    `json:"workspace,omitempty"`
	HoldSeconds          float64   `json:"hold_seconds"`
	QueuePosition        int       `json:"queue_position"`
	QueueSize            int       `json:"queue_size"`
	Schema               int       `json:"schema"`
	PID                  int       `json:"pid"`
	StartedAt            time.Time `json:"started_at"`
	At                   time.Time `json:"at"`
	Phase                string    `json:"phase"`
	CommandHash          string    `json:"command_sha256"`
	WaitSeconds          float64   `json:"wait_seconds"`
	RunSeconds           float64   `json:"run_seconds"`
	CommandUserSeconds   float64   `json:"command_user_seconds"`
	CommandSystemSeconds float64   `json:"command_system_seconds"`
}

func (e validationEvent) write(output, stderr io.Writer, phase string) {
	e.At = time.Now()
	e.Phase = phase
	if err := json.NewEncoder(output).Encode(e); err != nil {
		fmt.Fprintf(stderr, "write validation event: %v\n", err)
		return
	}
	if file, ok := output.(*os.File); ok {
		if err := file.Sync(); err != nil {
			fmt.Fprintf(stderr, "sync validation event: %v\n", err)
		}
	}
}

func newValidationEvent(started time.Time, commandHash string, stderr io.Writer) validationEvent {
	host, err := os.Hostname()
	if err != nil {
		fmt.Fprintf(stderr, "validation event hostname unavailable: %v\n", err)
	}
	return validationEvent{Schema: 1, RunID: rand.Text(), Host: host, Identifier: os.Getenv("DETENT_ISSUE_IDENTIFIER"), Workspace: os.Getenv("DETENT_WORKSPACE"), PID: os.Getpid(), StartedAt: started, CommandHash: commandHash}
}

// Observe the existing queue lookup without changing admission or polling.
func (e *validationEvent) observePosition(output, stderr io.Writer) func(context.Context, string, string) (int, int, error) {
	return func(ctx context.Context, path, name string) (int, int, error) {
		position, size, err := validationPosition(ctx, path, name)
		elapsed := time.Since(e.StartedAt).Seconds()
		if err == nil && (position != e.QueuePosition || size != e.QueueSize || elapsed-e.WaitSeconds >= 30) {
			e.QueuePosition, e.QueueSize = position, size
			e.WaitSeconds = elapsed
			e.write(output, stderr, "queued")
		}
		return position, size, err
	}
}
