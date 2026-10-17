package hubserver

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/update"
)

func (s *Service) runnerReleasePublished(ctx context.Context, version, platform, architecture string) bool {
	if _, err := update.CompareVersions(version, version); err != nil {
		return false
	}
	tag := "v" + strings.TrimPrefix(version, "v")
	key := tag + "/" + platform + "/" + architecture
	if _, ok := s.runnerPublishedReleases.Load(key); ok {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := s.config.RunnerReleaseClient
	release, err := client.GetRelease(ctx, tag)
	if err != nil || release.Draft || release.TagName != tag {
		return false
	}
	if update.VerifyPublishedRelease(ctx, client, release, platform, architecture, s.config.runnerReleaseSignatureVerifier) != nil {
		return false
	}
	s.runnerPublishedReleases.Store(key, true)
	return true
}
