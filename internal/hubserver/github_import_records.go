package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
)

func importJobResponse(row projectstore.GitHubImport) GitHubImport {
	return githubhosted.ImportResponse(row)
}

func (s *Service) githubImportRecords() githubhosted.ImportRecords {
	return githubhosted.ImportRecords{Repository: s.config.TenantRepository, Details: s.runDetailStore}
}

func (s *Service) readGitHubImportRecord(ctx context.Context, scope hubscope.NativeScope, id string) (GitHubImport, error) {
	return s.githubImportRecords().Read(ctx, scope, id)
}

func (s *Service) readGitHubImportRecords(ctx context.Context, scope hubscope.NativeScope, id string, after int64, limit int) ([]projectstore.GitHubImportRecord, error) {
	return s.githubImportRecords().List(ctx, scope, id, after, limit)
}

func (s *Service) decodeGitHubImportRecord(ctx context.Context, row projectstore.GitHubImportRecord) (GitHubImportRecord, error) {
	return s.githubImportRecords().Decode(ctx, row)
}
