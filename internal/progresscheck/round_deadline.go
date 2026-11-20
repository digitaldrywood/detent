package progresscheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/sessiontimeout"
)

var ErrRoundVerdictRejected = errors.New("round verdict publication rejected")

type RoundDeadlines struct {
	attempts int
	source   string
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

func (d *RoundDeadlines) Continue(ctx, sessionCtx context.Context, timeout *sessiontimeout.Error, diagnosis string, validate func(context.Context) error, retryDelay func(int) time.Duration) error {
	if timeout != nil {
		if err := d.Wait(sessionCtx, retryDelay); err != nil {
			return err
		}
	}
	if err := validate(ctx); err != nil {
		return err
	}
	d.current = ""
	d.recent = append(d.recent, diagnosis)
	if len(d.recent) > 6 {
		d.recent = d.recent[1:]
	}
	return nil
}

func (d *RoundDeadlines) WaitForSource(ctx context.Context, tree string, retryDelay func(int) time.Duration) error {
	if tree != "" {
		if d.source != "" && d.source != tree {
			d.attempts = 0
		}
		d.source = tree
	}
	return d.Wait(ctx, retryDelay)
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

func AwaitRoundAuthority(ctx context.Context, verify func(context.Context) error, retry func(error) error) error {
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		err := verify(ctx)
		if err == nil || retry == nil {
			return err
		}
		if stop := retry(err); stop != nil {
			return errors.Join(err, stop)
		}
	}
}

func RunRound(ctx context.Context, retry func(error) error, verify func(context.Context) error, publish func(context.Context, RoundInput) error, assess func(context.Context) (RoundAssessment, RoundInput, error)) (RoundAssessment, error) {
	var assessment RoundAssessment
	var input RoundInput
	for {
		if ctx.Err() != nil {
			return RoundAssessment{}, context.Cause(ctx)
		}
		bounded, cancel := context.WithTimeout(ctx, MaxDuration)
		var err error
		if retry != nil {
			err = verify(bounded)
		}
		if err == nil && input.RoundID == "" {
			assessment, input, err = assess(bounded)
		}
		if err == nil {
			if err = publish(bounded, input); err != nil {
				if errors.Is(err, ErrRoundVerdictRejected) {
					input = RoundInput{}
				}
				err = fmt.Errorf("publish round verdict: %w", err)
			}
		}
		cancel()
		if err == nil {
			return assessment, nil
		}
		if retry == nil || errors.Is(err, ErrChecklistSuperseded) {
			return RoundAssessment{}, err
		}
		if stop := retry(err); stop != nil {
			return RoundAssessment{}, errors.Join(err, stop)
		}
	}
}

func ProviderRoundOutcome(err, legacyDeadline error, assessment string) string {
	var timeout *sessiontimeout.Error
	var noProgress *NoProgressError
	var outcome string
	switch {
	case errors.As(err, &noProgress):
		outcome = "no_progress"
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
