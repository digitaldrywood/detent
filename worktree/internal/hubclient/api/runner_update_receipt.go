package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (c *Client) runnerReceiptPath() (string, error) {
	file, err := runnerauth.Load(c.IdentityFile())
	if err != nil {
		return "", err
	}
	base, err := RunnerOrganizationPath(file.Identity.OrganizationID)
	if err != nil {
		return "", err
	}
	return base + "/runners/" + url.PathEscape(file.Identity.RunnerID) + "/update/receipt", nil
}
func (c *Client) RunnerUpdateReceipt(ctx context.Context) (*runnerauth.UpdateReceipt, error) {
	path, err := c.runnerReceiptPath()
	if err != nil {
		return nil, err
	}
	var receipt *runnerauth.UpdateReceipt
	err = c.Request(ctx, http.MethodGet, path, nil, &receipt)
	return receipt, err
}
func (c *Client) StoreRunnerUpdateReceipt(ctx context.Context, receipt *runnerauth.UpdateReceipt, running runnerauth.BuildEvidence) error {
	path, err := c.runnerReceiptPath()
	if err != nil {
		return err
	}
	return c.Request(ctx, http.MethodPut, path, struct {
		Receipt *runnerauth.UpdateReceipt `json:"receipt"`
		Running runnerauth.BuildEvidence  `json:"running"`
	}{receipt, running}, nil)
}

func (c *Client) ReportRunnerLifecycle(ctx context.Context, report runnerauth.LifecycleReport) error {
	path, err := c.runnerReceiptPath()
	if err != nil {
		return err
	}
	return c.Request(ctx, http.MethodPost, strings.TrimSuffix(path, "/update/receipt")+"/lifecycle", report, nil)
}

func (c *Client) SetRunnerRoutingObserver(observe func(context.Context, string) error) {
	c.runnerRoutingMu.Lock()
	defer c.runnerRoutingMu.Unlock()
	c.runnerRoutingObserver = observe
}

func (c *Client) observeRunnerRouting(ctx context.Context, state string) error {
	c.runnerRoutingMu.RLock()
	observe := c.runnerRoutingObserver
	c.runnerRoutingMu.RUnlock()
	if observe == nil {
		return nil
	}
	return observe(ctx, state)
}
