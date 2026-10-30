package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

type goTestReceiptCandidates struct {
	Candidates []gotestreceipt.Candidate `json:"candidates"`
}

func (c *NativeClient) LookupGoTestReceipts(ctx context.Context, key string) ([]gotestreceipt.Candidate, error) {
	var response goTestReceiptCandidates
	if err := c.client.request(ctx, http.MethodGet, c.base()+"/go-test-receipts?key="+url.QueryEscape(key), nil, &response); err != nil {
		return nil, err
	}
	return response.Candidates, nil
}

func (c *NativeClient) RecordGoTestReceipt(ctx context.Context, receipt gotestreceipt.Receipt) error {
	return c.client.request(ctx, http.MethodPost, c.base()+"/go-test-receipts", receipt, nil)
}

type schedulerGoTestReceipts struct {
	scheduler *Scheduler
	project   string
}

func (s *Scheduler) GoTestReceipts(project string) gotestreceipt.Store {
	return schedulerGoTestReceipts{scheduler: s, project: project}
}

func (s schedulerGoTestReceipts) native() (*NativeClient, error) {
	connector := s.scheduler.nativeProject(s.project)
	if connector == nil || connector.client == nil {
		return nil, errors.New("project is not a native Hub project")
	}
	return connector.client, nil
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

type nativeGoTestReceipts struct {
	client *NativeClient
}

func (c *NativeClient) GoTestReceipts() gotestreceipt.Store {
	return nativeGoTestReceipts{client: c}
}

func (s nativeGoTestReceipts) Lookup(ctx context.Context, key string) ([]gotestreceipt.Candidate, error) {
	return s.client.LookupGoTestReceipts(ctx, key)
}

func (s nativeGoTestReceipts) Record(ctx context.Context, receipt gotestreceipt.Receipt) error {
	return s.client.RecordGoTestReceipt(ctx, receipt)
}

func (s nativeGoTestReceipts) Begin(ctx context.Context) (gotestreceipt.Execution, error) {
	return s.client.beginGoTestReceiptExecution(ctx)
}
func (s schedulerGoTestReceipts) Begin(ctx context.Context) (gotestreceipt.Execution, error) {
	native, err := s.native()
	if err != nil {
		return gotestreceipt.Execution{}, err
	}
	return native.beginGoTestReceiptExecution(ctx)
}
func (c *NativeClient) beginGoTestReceiptExecution(ctx context.Context) (gotestreceipt.Execution, error) {
	var execution gotestreceipt.Execution
	err := c.client.request(ctx, http.MethodPost, c.base()+"/go-test-receipts/executions", nil, &execution)
	return execution, err
}
