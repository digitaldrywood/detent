package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

func (c *NativeClient) AttestGoTestReceipts(ctx context.Context) (gotestreceipt.Attestation, error) {
	var attestation gotestreceipt.Attestation
	err := c.client.request(ctx, http.MethodPost, c.base()+"/go-test-receipts/attestations", nil, &attestation)
	return attestation, err
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

func (c *NativeClient) GoTestReceipts() gotestreceipt.Store {
	return nativeGoTestReceipts{client: c}
}

type goTestReceiptCandidates struct {
	Candidates []gotestreceipt.Candidate `json:"candidates"`
}

type nativeGoTestReceipts struct {
	client *NativeClient
}

func (s nativeGoTestReceipts) Attest(ctx context.Context) (gotestreceipt.Attestation, error) {
	return s.client.AttestGoTestReceipts(ctx)
}

func (s nativeGoTestReceipts) Lookup(ctx context.Context, key string) ([]gotestreceipt.Candidate, error) {
	return s.client.LookupGoTestReceipts(ctx, key)
}

func (s nativeGoTestReceipts) Record(ctx context.Context, receipt gotestreceipt.Receipt) error {
	return s.client.RecordGoTestReceipt(ctx, receipt)
}
