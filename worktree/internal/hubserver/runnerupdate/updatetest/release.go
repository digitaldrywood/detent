package updatetest

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/update"
)

type ReleaseClient struct {
	Release update.Release
	Files   map[string][]byte
	Reads   int
}

func (f *ReleaseClient) ListReleases(context.Context) ([]update.Release, error) {
	return nil, errors.New("heartbeat must select an exact release")
}

func (f *ReleaseClient) GetRelease(context.Context, string) (update.Release, error) {
	f.Reads++
	return f.Release, nil
}

func (f *ReleaseClient) Download(_ context.Context, name string) ([]byte, error) {
	if data, ok := f.Files[name]; ok {
		return data, nil
	}
	return nil, errors.New("asset unavailable")
}

var _ update.ReleaseClient = (*ReleaseClient)(nil)
