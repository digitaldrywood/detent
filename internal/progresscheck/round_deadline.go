package progresscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
)

type RoundDeadlines struct {
	attempts int
	filed    map[sessiontimeout.Kind]bool
	recent   []string
	current  string
}

func (d *RoundDeadlines) Observe(phase string, timeout *sessiontimeout.Error) {
	if timeout != nil {
		d.current = phase + ": " + timeout.Error()
	}
}

func (d *RoundDeadlines) Evidence() string {
	return strings.TrimSpace(strings.Join(d.recent, "\n") + "\n" + d.current)
}

func (d *RoundDeadlines) Continue(ctx, sessionCtx context.Context, timeout *sessiontimeout.Error, diagnosis, assessment string, validate func(context.Context) error, report func(context.Context, *sessiontimeout.Error) error, retryDelay func(int) time.Duration) (string, error) {
	if timeout != nil {
		if slices.Contains(d.recent, diagnosis) {
			if !d.filed[timeout.Kind] {
				if err := validate(ctx); err != nil {
					return "", err
				}
				if err := report(ctx, timeout); err != nil {
					return "", err
				}
				if d.filed == nil {
					d.filed = make(map[sessiontimeout.Kind]bool)
				}
				d.filed[timeout.Kind] = true
			}
			assessment = "The repeated timeout is recorded as a runner incident. Choose a different bounded check that can establish the next acceptance criterion. Preserve the existing source edits."
		}
		d.attempts++
		timer := time.NewTimer(retryDelay(d.attempts))
		select {
		case <-sessionCtx.Done():
			timer.Stop()
			return "", context.Cause(sessionCtx)
		case <-timer.C:
		}
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	d.current = ""
	d.recent = append(d.recent, diagnosis)
	if len(d.recent) > 6 {
		d.recent = d.recent[1:]
	}
	return assessment, nil
}

func RepeatedTimeoutIncident(phase, backend string, timeout *sessiontimeout.Error) ([]byte, error) {
	fingerprint := "provider-timeout:" + policy.Digest([]byte(phase+"\n"+backend+"\n"+string(timeout.Kind)))
	return json.Marshal(map[string]string{
		"title":       "Provider session repeatedly exceeds its deadline",
		"body":        fmt.Sprintf("The %s backend repeatedly reported %s during %s. The same recovery instruction returned after more than one retired session.", backend, timeout.Kind, phase),
		"fingerprint": fingerprint,
	})
}
