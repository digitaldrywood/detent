package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type validationEvent struct {
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
	}
}
