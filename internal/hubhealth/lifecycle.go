package hubhealth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Detector struct {
	cancel    context.CancelFunc
	done      chan struct{}
	Contracts ContractCache `json:"-"`
}

func StartDetector(evaluate func(context.Context) error, logger *slog.Logger) *Detector {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Detector{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := evaluate(ctx); err != nil && ctx.Err() == nil {
					logger.Warn("health detector evaluation failed", "error", err)
				}
			}
		}
	}()
	return d
}
func (d *Detector) Stop()                 { d.cancel(); <-d.done }
func (d *Detector) Done() <-chan struct{} { return d.done }
func (d *Store) EvaluateOrganizations(ctx context.Context, evaluate func(context.Context, tracker.OrganizationID, time.Time) error) error {
	rows, err := d.reader.QueryContext(ctx, "SELECT id FROM organizations ORDER BY id LIMIT ?", ReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	organizations := []tracker.OrganizationID{}
	for rows.Next() {
		var id tracker.OrganizationID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		organizations = append(organizations, id)
		if len(organizations) > ReadLimit {
			return ErrReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, organization := range organizations {
		failures = errors.Join(failures, evaluate(ctx, organization, time.Now().UTC()))
	}
	return failures
}
