package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/detailbatch"
)

func (s *Service) detailBatchService() *detailbatch.Service {
	return detailbatch.New(detailbatch.Deps{Store: s.runDetailStore})
}
func (s *Service) withRequestDetails(ctx context.Context) context.Context {
	return s.detailBatchService().WithRequest(ctx)
}
func (s *Service) runDetailStoreFor(ctx context.Context) attachment.DetailStore {
	return s.detailBatchService().StoreFor(ctx)
}
func (s *Service) prefetchDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) {
	s.detailBatchService().Prefetch(ctx, organization, refs)
}
