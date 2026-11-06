package dispatch

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

type schedulerGoTestReceipts struct {
	scheduler *Scheduler
	project   string
}

func (s *Scheduler) GoTestReceipts(project string) gotestreceipt.Store {
	return schedulerGoTestReceipts{scheduler: s, project: project}
}

func (s schedulerGoTestReceipts) native() (*NativeClient, error) {
	connector := s.scheduler.nativeProject(s.project)
	if connector == nil || connector.NativeClient() == nil {
		return nil, errors.New("project is not a native Hub project")
	}
	return connector.NativeClient(), nil
}

func (s schedulerGoTestReceipts) Attest(ctx context.Context) (gotestreceipt.Attestation, error) {
	native, err := s.native()
	if err != nil {
		return gotestreceipt.Attestation{}, err
	}
	return native.AttestGoTestReceipts(ctx)
}

func (s schedulerGoTestReceipts) Lookup(ctx context.Context, key string) ([]gotestreceipt.Candidate, error) {
	native, err := s.native()
	if err != nil {
		return nil, err
	}
	return native.LookupGoTestReceipts(ctx, key)
}

func (s schedulerGoTestReceipts) Record(ctx context.Context, receipt gotestreceipt.Receipt) error {
	native, err := s.native()
	if err != nil {
		return err
	}
	return native.RecordGoTestReceipt(ctx, receipt)
}
