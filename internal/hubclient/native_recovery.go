package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Attempts(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeAttempt], error) {
	return c.AttemptsPage(ctx, id, cursor, 100)
}

func (c *NativeClient) blockerPage(ctx context.Context, id tracker.NativeWorkItemID, kind, cursor string, target any) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	params := url.Values{"view": {"blockers"}, "limit": {"100"}, "cursor": {cursor}}
	return c.client.request(ctx, http.MethodGet, c.base()+path+"/"+kind+"?"+params.Encode(), nil, target)
}

func (c *NativeClient) Recovery(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"?view=recovery&limit=1", nil, &result)
	if err == nil && result.Continuation != nil {
		result, err = c.continuedRecovery(ctx, path, result.Continuation)
	}
	if err == nil && (result.Issue.OrganizationID != c.organization || result.Issue.ProjectID != c.project || result.Issue.WorkItemID != id) {
		return result, errors.New("read recovery: scoped work item is missing")
	}
	return result, err
}

func (c *NativeClient) continuedRecovery(ctx context.Context, path string, first *tracker.NativeRecoveryPage) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	reader := &nativeRecoveryReader{page: first, digest: first.SHA256, total: first.TotalBytes, readPage: func(cursor string) (*tracker.NativeRecoveryPage, error) {
		var next tracker.NativeRecovery
		params := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {cursor}}
		if err := c.client.request(ctx, http.MethodGet, c.base()+path+"?"+params.Encode(), nil, &next); err != nil {
			return nil, err
		}
		return next.Continuation, nil
	}}
	hash := sha256.New()
	decoder := json.NewDecoder(io.TeeReader(reader, hash))
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("%w: decode recovery continuation: %w", ErrUnavailable, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return result, fmt.Errorf("%w: trailing recovery document", ErrUnavailable)
		}
		return result, fmt.Errorf("%w: incomplete recovery continuation: %w", ErrUnavailable, err)
	}
	if result.Continuation != nil || reader.offset != reader.total || hex.EncodeToString(hash.Sum(nil)) != reader.digest {
		return result, fmt.Errorf("%w: recovery continuation digest mismatch", ErrUnavailable)
	}
	return result, nil
}

type nativeRecoveryReader struct {
	readPage func(string) (*tracker.NativeRecoveryPage, error)
	page     *tracker.NativeRecoveryPage
	data     *bytes.Reader
	digest   string
	total    int64
	offset   int64
}

func (r *nativeRecoveryReader) Read(p []byte) (int, error) {
	for {
		if r.data != nil && r.data.Len() > 0 {
			return r.data.Read(p)
		}
		if r.page == nil {
			return 0, io.EOF
		}
		page := r.page
		if r.data != nil {
			if page.NextCursor == "" {
				r.page = nil
				if r.offset != r.total {
					return 0, fmt.Errorf("%w: recovery continuation ended early", ErrUnavailable)
				}
				return 0, io.EOF
			}
			var err error
			page, err = r.readPage(page.NextCursor)
			if err != nil {
				return 0, err
			}
		}
		if page == nil || len(page.Data) == 0 || page.Offset != r.offset || page.TotalBytes != r.total || page.SHA256 != r.digest || len(r.digest) != 64 || r.total <= 0 || int64(len(page.Data)) > r.total-r.offset {
			return 0, fmt.Errorf("%w: recovery continuation did not progress or changed snapshot", ErrUnavailable)
		}
		r.offset += int64(len(page.Data))
		r.page = page
		r.data = bytes.NewReader(page.Data)
	}
}
