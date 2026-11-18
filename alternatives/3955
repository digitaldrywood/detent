package runnerupdate

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

func (s *Service) ReleasePublished(ctx context.Context, version, platform, architecture string) bool {
	if _, err := update.CompareVersions(version, version); err != nil {
		return false
	}
	tag := "v" + strings.TrimPrefix(version, "v")
	key := tag + "/" + platform + "/" + architecture
	if _, ok := s.deps.Published.Load(key); ok {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := s.deps.ReleaseClient
	release, err := client.GetRelease(ctx, tag)
	if err != nil || release.Draft || release.TagName != tag {
		return false
	}
	if update.VerifyPublishedRelease(ctx, client, release, platform, architecture, s.deps.VerifySignature) != nil {
		return false
	}
	s.deps.Published.Store(key, true)
	return true
}

func (s *Service) RecordWorkerHubVersion(previous *tracker.NativeRunData, current *tracker.NativeRunData) {
	if current.Runtime == nil || current.Runtime.WorkerLaunch == nil {
		return
	}
	runtime := *current.Runtime
	launch := *runtime.WorkerLaunch
	launch.HubVersion = s.deps.Version
	if previous != nil && previous.Runtime != nil && previous.Runtime.WorkerLaunch != nil {
		launch.HubVersion = previous.Runtime.WorkerLaunch.HubVersion
	}
	runtime.WorkerLaunch = &launch
	current.Runtime = &runtime
}
