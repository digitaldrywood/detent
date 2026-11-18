package attachmentsweep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

type Retention struct {
	Registry *platformstore.Repository
	Expired  func() (int, []byte, error)
	Remove   func(attachment.Metadata) error
	Sweeper  func(string) Sweep
}

func (r Retention) Visit(ctx context.Context, organization string, inventory bool) (bool, error) {
	if inventory {
		if err := r.expire(); err != nil {
			return false, err
		}
		err := r.Registry.VisitAttachmentPrefixes(ctx, organization, func(namespace, after string) (string, error) {
			endpoint := "/internal/v1/attachments/exists"
			if namespace == "attempt-diffs/" {
				endpoint = "/internal/v1/diff-bodies/exists"
			}
			return r.Sweeper(endpoint).Page(ctx, organization, namespace, after)
		})
		if err != nil {
			return false, err
		}
	}
	return r.Sweeper("/internal/v1/attachments/exists").Archives(ctx, organization, inventory)
}

func (r Retention) expire() error {
	status, raw, err := r.Expired()
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New("attachment retention metadata unavailable")
	}
	var records []attachment.Metadata
	if err := json.Unmarshal(raw, &records); err != nil {
		return err
	}
	for _, record := range records {
		if err := r.Remove(record); err != nil {
			return err
		}
	}
	return nil
}
