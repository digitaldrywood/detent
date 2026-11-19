package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeBoardCounts = operatorcommands.NativeBoardCounts
type nativeBoardResult = operatorcommands.NativeBoardResult

func (s *Service) executeBoardRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	deps := operatorcommands.BoardDeps{
		Now: s.config.Clock, ReadProject: s.readProjectRecord, ReadIssues: s.readIssues,
		NativeProject: func(ctx context.Context, record projectstore.Record) (tracker.NativeProject, error) {
			return s.projectRecordNative(ctx, record)
		},
		ListProjects: func(ctx context.Context, scope hubscope.NativeScope, limit int) ([]tracker.NativeProject, bool, error) {
			page, err := s.operatorProjectList(ctx, scope, operatortool.ProjectReadRequest{Limit: limit})
			return page.Projects, page.NextCursor != "", err
		},
	}
	return deps.Read(ctx, call)
}
