package remoterunner

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type UpdateReceipts struct {
	load func() (*runnerauth.UpdateReceipt, error)
	save func(*runnerauth.UpdateReceipt) error
}

func NewUpdateReceipts(ctx context.Context, client *api.Client, running runnerauth.BuildEvidence) *UpdateReceipts {
	return &UpdateReceipts{
		load: func() (*runnerauth.UpdateReceipt, error) {
			loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return client.RunnerUpdateReceipt(loadCtx)
		},
		save: func(receipt *runnerauth.UpdateReceipt) error {
			saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return client.StoreRunnerUpdateReceipt(saveCtx, receipt, running)
		},
	}
}

func (s *UpdateReceipts) Load() (*runnerauth.UpdateReceipt, error) {
	return s.load()
}

func (s *UpdateReceipts) Save(receipt *runnerauth.UpdateReceipt) error {
	return s.save(receipt)
}
