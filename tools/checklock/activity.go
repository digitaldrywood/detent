package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

type validationActivity struct {
	Owner instancelock.Owner `json:"owner"`
	Phase string             `json:"phase"`
	At    time.Time          `json:"at"`
	Bytes int64              `json:"bytes"`
}

type validationActivityRecorder struct {
	mu        sync.Mutex
	path      string
	activity  validationActivity
	published time.Time
	stderr    io.Writer
}

func newValidationActivityRecorder(path string, stderr io.Writer) *validationActivityRecorder {
	r := &validationActivityRecorder{path: path + ".activity", stderr: stderr}
	owner, err := instancelock.Inspect(path)
	if err != nil || owner.MetadataError != nil || owner.Status != instancelock.StatusHeld {
		fmt.Fprintln(stderr, "validation owner activity unavailable: cannot identify lock owner")
		return r
	}
	r.activity = validationActivity{Owner: owner.Owner, Phase: "command", At: time.Now()}
	r.publish()
	return r
}

func (r *validationActivityRecorder) publish() {
	data, err := json.Marshal(r.activity)
	if err == nil {
		err = os.WriteFile(r.path+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(r.path+".tmp", r.path)
	}
	if err != nil {
		fmt.Fprintln(r.stderr, "validation owner activity unavailable: cannot publish activity")
	}
	r.published = time.Now()
}

func (r *validationActivityRecorder) observe(data []byte) {
	if r.activity.Owner.PID == 0 || len(data) == 0 {
		return
	}
	phase := r.activity.Phase
	if phase == "command" {
		phase = "running"
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, marker := range []struct{ command, phase string }{
			{"go build ", "build"},
			{"templ generate", "generate"},
			{"golangci-lint", "lint"},
			{"go vet ", "vet"},
			{"nilaway", "nilaway"},
			{"scripts/test-race-cover.sh", "tests"},
			{"go run ./tools/covercheck", "coverage"},
		} {
			if strings.Contains(line, marker.command) {
				phase = marker.phase
			}
		}
	}
	changed := phase != r.activity.Phase
	r.activity.Phase = phase
	r.activity.At = time.Now()
	r.activity.Bytes += int64(len(data))
	if changed || time.Since(r.published) >= time.Second {
		r.publish()
	}
}

type validationActivityWriter struct {
	output   io.Writer
	recorder *validationActivityRecorder
}

func (w validationActivityWriter) Write(data []byte) (int, error) {
	w.recorder.mu.Lock()
	defer w.recorder.mu.Unlock()
	w.recorder.observe(data)
	return w.output.Write(data)
}

func readValidationActivity(path string, owner instancelock.Inspection) (validationActivity, bool) {
	if owner.Status != instancelock.StatusHeld || owner.MetadataError != nil {
		return validationActivity{}, false
	}
	data, err := os.ReadFile(path + ".activity")
	var activity validationActivity
	if err != nil || json.Unmarshal(data, &activity) != nil || activity.Owner != owner.Owner || activity.At.Before(owner.Owner.StartedAt) || activity.At.After(time.Now()) {
		return validationActivity{}, false
	}
	return activity, true
}
