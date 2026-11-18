package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/update"
)

type runnerReleaseFixture struct {
	release update.Release
	files   map[string][]byte
	reads   int
}

func (f *runnerReleaseFixture) ListReleases(context.Context) ([]update.Release, error) {
	return nil, errors.New("heartbeat must select an exact release")
}

func (f *runnerReleaseFixture) GetRelease(context.Context, string) (update.Release, error) {
	f.reads++
	return f.release, nil
}

func (f *runnerReleaseFixture) Download(_ context.Context, name string) ([]byte, error) {
	if data, ok := f.files[name]; ok {
		return data, nil
	}
	return nil, errors.New("asset unavailable")
}
