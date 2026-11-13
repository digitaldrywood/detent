package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessreceipts"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accessReceiptsService() *accessreceipts.Service {
	return accessreceipts.New(accessreceipts.Deps{Repository: s.config.AccessRepository, TenantRepository: s.config.TenantRepository, Organization: tracker.OrganizationID(s.appOrganizationID()), Now: s.config.now, Details: s.runDetailStore(), ResolveJSON: func(ctx context.Context, raw []byte) ([]byte, error) {
		return resolveStorageJSON(s.contentContext(ctx), raw)
	}})
}
func (s *Service) accessReceiptCommand(ctx context.Context, c hostedCommand) accessreceipts.Command {
	return accessreceipts.Command{Organization: tracker.OrganizationID(s.commandOrganization(ctx, c)), Actor: c.actor, Operation: c.operation, Key: c.key, Input: c.input}
}
func (s *Service) readHostedOperationRecord(ctx context.Context, c hostedCommand) (json.RawMessage, bool, error) {
	raw, found, err := s.accessReceiptsService().Read(ctx, s.accessReceiptCommand(ctx, c))
	return raw, found, hubapi.FromFeatureError(err)
}
func (s *Service) claimHostedOperationRecord(ctx context.Context, c hostedCommand) (bool, json.RawMessage, error) {
	claimed, raw, err := s.accessReceiptsService().Claim(ctx, s.accessReceiptCommand(ctx, c))
	return claimed, raw, hubapi.FromFeatureError(err)
}
func (s *Service) completeHostedOperationRecord(ctx context.Context, c hostedCommand, value any) (json.RawMessage, error) {
	raw, err := s.accessReceiptsService().Complete(ctx, s.accessReceiptCommand(ctx, c), value)
	return raw, hubapi.FromFeatureError(err)
}
func (s *Service) abandonHostedOperationRecord(ctx context.Context, c hostedCommand) error {
	return hubapi.FromFeatureError(s.accessReceiptsService().Abandon(ctx, s.accessReceiptCommand(ctx, c)))
}
func (s *Service) retainHostedOperationRecords(ctx context.Context, cutoff time.Time) error {
	return hubapi.FromFeatureError(s.accessReceiptsService().Retain(ctx, cutoff))
}
