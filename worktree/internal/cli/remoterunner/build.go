package remoterunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func RunningBuild(ctx context.Context, info buildinfo.Info, version string) runnerauth.BuildEvidence {
	if info.Version == "" {
		info.Version = version
	}
	info = buildinfo.Normalize(info)
	build := runnerauth.BuildEvidence{Version: info.Version, Commit: info.Commit, Source: "unknown", OS: runtime.GOOS, Architecture: runtime.GOARCH, ObservedAt: time.Now().UTC()}
	path, err := os.Executable()
	if err == nil {
		build.Source = string(detentupdate.DetectInstallSource(detentupdate.DetectionOptions{ExecutablePath: path, CurrentVersion: info.Version, GOOS: runtime.GOOS}).Source)
		file, err := os.Open(path)
		if err == nil {
			hash := sha256.New()
			_, readErr := io.Copy(hash, file)
			closeErr := file.Close()
			if readErr == nil && closeErr == nil {
				build.SHA256 = hex.EncodeToString(hash.Sum(nil))
			}
		}
	}
	patched := info.Dirty
	if metadata, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range metadata.Settings {
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				patched = true
			}
		}
		for _, dependency := range metadata.Deps {
			if dependency.Replace != nil {
				patched = true
			}
		}
	}
	if patched {
		build.Source = "private_patched_source"
	} else if err == nil {
		build.VerifiedRelease = detentupdate.InstalledReleaseVerified(ctx, detentupdate.DetectionOptions{ExecutablePath: path, CurrentVersion: info.Version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, info.Commit, build.SHA256)
		if build.VerifiedRelease {
			build.Source = "release"
		}
	}
	return build
}
