package api

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strconv"

	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func (c *NativeClient) RecordAuditDisposition(ctx context.Context, request securityaudit.DispositionCommand) (securityaudit.Disposition, error) {
	if request.RequestID == "" {
		request.RequestID = uuid.NewString()
	}
	var result securityaudit.Disposition
	err := c.client.Request(ctx, http.MethodPost, c.base()+"/security-audits/dispositions", request, &result)
	return result, err
}

func (c *NativeClient) AuditEvidence(ctx context.Context, key securityaudit.Key) (securityaudit.Evidence, error) {
	var value securityaudit.Evidence
	query := url.Values{"repository": {key.Repository}, "pull_request": {strconv.Itoa(key.PRNumber)}, "base_sha": {key.BaseSHA}, "head_sha": {key.HeadSHA}}
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/security-audits/evidence?"+query.Encode(), nil, &value)
	return value, err
}

func (c *NativeClient) ListSecurityAuditDispositions(ctx context.Context, id int64) ([]securityaudit.Disposition, error) {
	var value []securityaudit.Disposition
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/security-audits/"+strconv.FormatInt(id, 10)+"/dispositions", nil, &value)
	return value, err
}
