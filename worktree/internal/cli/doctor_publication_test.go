package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestCheckDoctorPublicIssueExposure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sourceInfo ghconnector.RepositoryInfo
		destInfo   ghconnector.RepositoryInfo
		destErr    error
		override   bool
		wantStatus doctorStatus
		wantDetail string
	}{
		{name: "private source public destination warns", sourceInfo: ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, destInfo: ghconnector.RepositoryInfo{Visibility: "public"}, wantStatus: doctorWarn, wantDetail: "private source private/source files into public destinations"},
		{name: "private destination is safe", sourceInfo: ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, destInfo: ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, wantStatus: doctorOK, wantDetail: "do not file"},
		{name: "public source is safe", sourceInfo: ghconnector.RepositoryInfo{Visibility: "public"}, destInfo: ghconnector.RepositoryInfo{Visibility: "public"}, wantStatus: doctorOK, wantDetail: "sources are not private"},
		{name: "unknown destination warns", sourceInfo: ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, destErr: errors.New("unavailable"), wantStatus: doctorWarn, wantDetail: "could not be determined"},
		{name: "operator override is explicit", sourceInfo: ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, destInfo: ghconnector.RepositoryInfo{Visibility: "public"}, override: true, wantStatus: doctorOK, wantDetail: "do not file"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerGitHub
			cfg.Tracker.Repository = "private/source"
			cfg.Retro.Enabled = true
			cfg.Retro.ProductRepository = "public/destination"
			cfg.Retro.AllowPublicCrossProjectDetails = test.override
			check := checkDoctorPublicIssueExposure(context.Background(), "private", cfg, doctorDeps{
				githubRepositoryInfo: func(_ context.Context, _ workflowconfig.Config, repository string) (ghconnector.RepositoryInfo, error) {
					if repository == "private/source" {
						return test.sourceInfo, nil
					}
					return test.destInfo, test.destErr
				},
			})
			if check.Status != test.wantStatus || !strings.Contains(check.Detail, test.wantDetail) {
				t.Fatalf("check = %#v, want status %s and detail containing %q", check, test.wantStatus, test.wantDetail)
			}
		})
	}
}
