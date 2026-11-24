package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
)

var (
	ErrHostedDatabaseBinding     = errors.New("hosted database organization binding is invalid")
	ErrHostedMetadataUnavailable = errors.New("hosted metadata is unavailable")
)

type hostedMetadataResponse struct {
	OrganizationID         string                        `json:"organization_id"`
	ProviderOrganizationID string                        `json:"provider_organization_id"`
	Members                *int64                        `json:"members"`
	Projects               *int64                        `json:"projects"`
	Runners                *int64                        `json:"runners"`
	Events                 *int64                        `json:"events"`
	LastActivity           *time.Time                    `json:"last_activity"`
	DatabaseBytes          *int64                        `json:"database_bytes"`
	Healthy                bool                          `json:"healthy"`
	UnavailableUsage       []string                      `json:"unavailable_usage"`
	Entitlement            hostedplans.HostedEntitlement `json:"entitlement"`
	PlanID                 string                        `json:"plan_id"`
	StorageQuotaBytes      int64                         `json:"storage_quota_bytes,omitempty"`
	EventQuota             int64                         `json:"event_quota,omitempty"`
	CostDrivers            struct {
		ActiveIssues          *int64           `json:"active_issues"`
		ArchivedIssues        *int64           `json:"archived_issues"`
		DatabaseBytes         *int64           `json:"database_bytes"`
		WALBytes              *int64           `json:"wal_bytes"`
		Requests              *int64           `json:"requests"`
		RequestBytes          *int64           `json:"request_bytes"`
		ResponseBytes         *int64           `json:"response_bytes"`
		ArtifactRetainedBytes *int64           `json:"artifact_retained_bytes"`
		ArtifactReservedBytes *int64           `json:"artifact_reserved_bytes"`
		RelayBytes            *int64           `json:"relay_bytes"`
		WindowEndsAt          time.Time        `json:"window_ends_at"`
		OperatorAI            chatUsageSummary `json:"operator_ai"`
		OperatorAICostUSD     *float64         `json:"operator_ai_cost_usd"`
	} `json:"cost_drivers"`
}

func (s *Service) readHostedMetadata(ctx context.Context) (hostedMetadataResponse, error) {
	if s.config.Hosted == nil || s.database == nil {
		return hostedMetadataResponse{}, ErrHostedMetadataUnavailable
	}
	health, status := s.readInstanceHealth(ctx)
	if status != http.StatusOK {
		return hostedMetadataResponse{}, ErrHostedMetadataUnavailable
	}
	usage, err := s.operatorBillingUsage(ctx, s.config.Clock())
	if err != nil {
		return hostedMetadataResponse{}, ErrHostedMetadataUnavailable
	}
	unavailable := append(slices.Clone(usage.UnavailableUsage), "database_bytes", "wal_bytes")
	slices.Sort(unavailable)
	report := hostedMetadataResponse{
		OrganizationID:         usage.OrganizationID,
		ProviderOrganizationID: usage.ProviderOrganizationID,
		Members:                usage.Members,
		Projects:               usage.Projects,
		Runners:                usage.Runners,
		Events:                 usage.Events,
		LastActivity:           usage.LastActivity,
		Healthy:                health.Status == "ok",
		UnavailableUsage:       slices.Compact(unavailable),
		Entitlement:            usage.Entitlement,
		PlanID:                 usage.PlanID,
		StorageQuotaBytes:      usage.StorageQuotaBytes,
		EventQuota:             usage.EventQuota,
	}
	report.CostDrivers.ActiveIssues = usage.CostDrivers.ActiveIssues
	report.CostDrivers.ArchivedIssues = usage.CostDrivers.ArchivedIssues
	report.CostDrivers.Requests = usage.CostDrivers.Requests
	report.CostDrivers.RequestBytes = usage.CostDrivers.RequestBytes
	report.CostDrivers.ResponseBytes = usage.CostDrivers.ResponseBytes
	report.CostDrivers.ArtifactRetainedBytes = usage.CostDrivers.ArtifactRetainedBytes
	report.CostDrivers.ArtifactReservedBytes = usage.CostDrivers.ArtifactReservedBytes
	report.CostDrivers.RelayBytes = usage.CostDrivers.RelayBytes
	report.CostDrivers.WindowEndsAt = usage.CostDrivers.WindowEndsAt
	report.CostDrivers.OperatorAI = usage.CostDrivers.OperatorAI
	report.CostDrivers.OperatorAICostUSD = usage.CostDrivers.OperatorAICostUSD
	return report, nil
}
