package progresscheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

const (
	ToolLoopWindow      = 64
	toolLoopSteerAt     = 3
	toolLoopStopAt      = 6
	maxFingerprintBytes = 1 << 20
)

type ToolExperiment struct {
	Tool         string
	Arguments    json.RawMessage
	WorkspaceDir string
	Status       string
	ExitCode     *int
	Result       json.RawMessage
}

func (e ToolExperiment) Fingerprint() string {
	if e.Tool == "" || e.Status == "" || len(e.Arguments)+len(e.Result) > maxFingerprintBytes {
		return ""
	}
	var ok bool
	if e.Arguments, ok = canonicalToolJSON(e.Arguments); !ok {
		return ""
	}
	if e.Result, ok = canonicalToolJSON(e.Result); !ok {
		return ""
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func canonicalToolJSON(raw json.RawMessage) (json.RawMessage, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

type ToolLoopAction string

const (
	ToolLoopSteer ToolLoopAction = "steer"
	ToolLoopStop  ToolLoopAction = "stop"
)

type ToolLoopEvidence struct {
	Action      ToolLoopAction `json:"action"`
	Fingerprint string         `json:"fingerprint"`
	Period      int            `json:"period"`
	Repeats     int            `json:"repeats"`
}

type NoProgressError struct {
	Evidence ToolLoopEvidence
}

func (e *NoProgressError) Error() string {
	return fmt.Sprintf("provider stopped after %d unchanged repetitions of a %d-call tool sequence", e.Evidence.Repeats, e.Evidence.Period)
}

type toolDecision struct {
	fingerprint string
	sequence    uint64
}

type toolSteer struct {
	first        uint64
	acknowledged uint64
}

type ToolLoop struct {
	history []toolDecision
	steers  map[string]toolSteer
}

func (l *ToolLoop) Observe(sequence uint64, fingerprint string) ToolLoopEvidence {
	if sequence == 0 || len(l.history) > 0 && sequence <= l.history[len(l.history)-1].sequence {
		return ToolLoopEvidence{}
	}
	if len(l.history) == ToolLoopWindow {
		copy(l.history, l.history[1:])
		l.history = l.history[:ToolLoopWindow-1]
	}
	l.history = append(l.history, toolDecision{fingerprint: fingerprint, sequence: sequence})
	for key, steer := range l.steers {
		if steer.first < l.history[0].sequence {
			delete(l.steers, key)
		}
	}
	if fingerprint == "" {
		return ToolLoopEvidence{}
	}
	for period := 1; period <= 4; period++ {
		repeats := l.repeatedSuffix(period)
		if repeats < toolLoopSteerAt {
			continue
		}
		key := l.cycleKey(period)
		evidence := ToolLoopEvidence{Fingerprint: key, Period: period, Repeats: repeats}
		steer, exists := l.steers[key]
		if !exists {
			if l.steers == nil {
				l.steers = make(map[string]toolSteer)
			}
			l.steers[key] = toolSteer{first: sequence}
			evidence.Action = ToolLoopSteer
			return evidence
		}
		if repeats >= toolLoopStopAt && steer.acknowledged != 0 && l.history[len(l.history)-toolLoopSteerAt*period].sequence > steer.acknowledged {
			evidence.Action = ToolLoopStop
			return evidence
		}
		return ToolLoopEvidence{}
	}
	return ToolLoopEvidence{}
}

func (l *ToolLoop) Acknowledge(fingerprint string, lastStarted uint64) {
	steer, exists := l.steers[fingerprint]
	if exists && steer.acknowledged == 0 && lastStarted >= steer.first {
		steer.acknowledged = lastStarted
		l.steers[fingerprint] = steer
	}
}

func (l *ToolLoop) repeatedSuffix(period int) int {
	end := len(l.history)
	if end < period {
		return 0
	}
	cycle := l.history[end-period:]
	for _, decision := range cycle {
		if decision.fingerprint == "" {
			return 0
		}
	}
	for start := end - period; start >= 0; start -= period {
		if !slices.EqualFunc(cycle, l.history[start:start+period], func(a, b toolDecision) bool {
			return a.fingerprint == b.fingerprint
		}) {
			return (end - start - period) / period
		}
	}
	return end / period
}

func (l *ToolLoop) cycleKey(period int) string {
	cycle := l.history[len(l.history)-period:]
	var key string
	for start := range cycle {
		var rotation string
		for offset := range cycle {
			rotation += cycle[(start+offset)%period].fingerprint + "\x00"
		}
		if key == "" || rotation < key {
			key = rotation
		}
	}
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}
