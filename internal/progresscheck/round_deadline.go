package progresscheck

import (
	"context"
	"encoding/json"
	"errors"
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
		if err := d.Wait(sessionCtx, retryDelay); err != nil {
			return "", err
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

func (d *RoundDeadlines) Wait(ctx context.Context, retryDelay func(int) time.Duration) error {
	d.attempts++
	timer := time.NewTimer(retryDelay(d.attempts))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return ctx.Err()
	}
}

func RunRound(ctx context.Context, retry func(error) error, publish func(context.Context, RoundInput) error, assess func(context.Context) (RoundAssessment, RoundInput, error)) (RoundAssessment, error) {
	var assessment RoundAssessment
	var input RoundInput
	for {
		if ctx.Err() != nil {
			return RoundAssessment{}, context.Cause(ctx)
		}
		bounded, cancel := context.WithTimeout(ctx, MaxDuration)
		var err error
		if input.RoundID == "" {
			assessment, input, err = assess(bounded)
		}
		if err == nil {
			if err = publish(bounded, input); err != nil {
				err = fmt.Errorf("publish round verdict: %w", err)
			}
		}
		cancel()
		if err == nil {
			return assessment, nil
		}
		if retry == nil {
			return RoundAssessment{}, err
		}
		if stop := retry(err); stop != nil {
			return RoundAssessment{}, errors.Join(err, stop)
		}
	}
}

func ProviderRoundOutcome(err, legacyDeadline error, assessment string) string {
	var timeout *sessiontimeout.Error
	var outcome string
	switch {
	case errors.As(err, &timeout):
		outcome = string(timeout.Kind)
	case errors.Is(err, legacyDeadline):
		outcome = "turn_review_deadline"
	default:
		return ""
	}
	if assessment == "" {
		return outcome
	}
	return outcome + ": " + assessment
}

func RepeatedTimeoutIncident(phase, backend string, timeout *sessiontimeout.Error) ([]byte, error) {
	fingerprint := "provider-timeout:" + policy.Digest([]byte(phase+"\n"+backend+"\n"+string(timeout.Kind)))
	return json.Marshal(map[string]string{
		"title":       "Provider session repeatedly exceeds its deadline",
		"body":        fmt.Sprintf("The %s backend repeatedly reported %s during %s. The same recovery instruction returned after more than one retired session.", backend, timeout.Kind, phase),
		"fingerprint": fingerprint,
	})
}
