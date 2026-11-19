package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) coordinatorSlackIntegration(ctx context.Context, organization tracker.OrganizationID) (slackIntegrationStatus, error) {
	reader, ok := s.config.TenantRepository.(interface {
		ReadSlack(context.Context, tracker.OrganizationID, corestore.Workload, func(postgres.SlackReader) error) error
	})
	if !ok {
		return slackIntegrationStatus{}, operatortool.ErrServiceUnavailable
	}
	var result slackIntegrationStatus
	err := reader.ReadSlack(ctx, organization, corestore.Serving, func(r postgres.SlackReader) error {
		row, err := r.SlackIntegration(ctx)
		if err == nil {
			if row.OrganizationID != organization {
				return corestore.ErrTenantMismatch
			}
			result.ChannelName = row.ChannelName
			result.LastDeliveryFailed, result.LastStatusCode = row.LastDeliveryFailed, int(row.LastStatusCode)
			if row.LastSuccessAt != nil {
				stamp := hubapi.FormatHubTime(*row.LastSuccessAt)
				result.LastSuccessAt = &stamp
			}
			if row.LastFailureAt != nil {
				stamp := hubapi.FormatHubTime(*row.LastFailureAt)
				result.LastFailureAt = &stamp
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		secret, err := r.AccessSecret(ctx, "", slackWebhookSecret)
		if err == nil {
			if secret.OrganizationID != organization || secret.ProjectID != "" || secret.Kind != slackWebhookSecret {
				return corestore.ErrTenantMismatch
			}
			result.Webhook = maskedSlackWebhook
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Service) coordinatorProjectRunners(ctx context.Context, record conversationRecord, credential hubapi.APICredential, editable bool) (any, error) {
	if s.config.Hosted == nil || string(record.OrganizationID) != s.config.Hosted.OrganizationID {
		return nil, operatortool.ErrAccessDenied
	}
	projects, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return nil, err
	}
	fleet, reservations, err := s.readHostedFleetSnapshotTenant(ctx, credential, projects, record.ProjectID, s.config.Clock().UTC())
	if err != nil {
		return nil, err
	}
	attached := make([]hostedFleetRunner, 0, len(fleet.Runners))
	connected := map[string]bool{}
	connectionHealth := map[string]string{}
	for _, r := range fleet.Runners {
		connected[r.ID] = r.State == "active" && r.ConnectionHealth == "online"
		connectionHealth[r.ID] = r.ConnectionHealth
		if r.Routing != nil {
			routing := *r.Routing
			routing.ProjectIDs = slices.DeleteFunc(slices.Clone(routing.ProjectIDs), func(project tracker.ProjectID) bool { return project != record.ProjectID })
			routing.ProjectRanks = map[tracker.ProjectID]int{record.ProjectID: routing.ProjectRanks[record.ProjectID]}
			r.Routing = &routing
		}
		r.Problems = slices.DeleteFunc(slices.Clone(r.Problems), func(p runnerauth.Problem) bool { return p.ProjectID != "" && p.ProjectID != string(record.ProjectID) })
		if r.Capacity != nil {
			capacity := *r.Capacity
			capacity.Projects = maps.Clone(capacity.Projects)
			for project := range capacity.Projects {
				if project != record.ProjectID {
					delete(capacity.Projects, project)
				}
			}
			r.Capacity = &capacity
		}
		attached = append(attached, r)
	}
	scopeHostUsage(attached)
	scopeProviderUsage(attached, reservations)
	type projectRunner struct {
		hostedFleetRunner
		Connected        bool   `json:"connected"`
		ConnectionHealth string `json:"connection_health"`
	}
	runners := make([]projectRunner, 0, len(attached))
	for _, r := range attached {
		runners = append(runners, projectRunner{r, connected[r.ID], connectionHealth[r.ID]})
	}
	return map[string]any{"project_id": record.ProjectID, "usage_scope": "project", "runners": runners, "observed_at": s.config.Clock(), "runner_options": []string{"self-hosted", "Sprites"}, "runner_details_access": editable}, nil
}

func coordinatorProjectMembers(raw json.RawMessage, project tracker.ProjectID) (any, error) {
	var page struct {
		Members     operatoradmin.PageResult[hostedMemberView]     `json:"members"`
		Invitations operatoradmin.PageResult[hostedInvitationView] `json:"invitations"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	for index := range page.Members.Items {
		page.Members.Items[index].Grants = slices.DeleteFunc(page.Members.Items[index].Grants, func(grant hostedMemberGrant) bool { return grant.ProjectID != string(project) })
	}
	for index := range page.Invitations.Items {
		page.Invitations.Items[index].Grants = slices.DeleteFunc(page.Invitations.Items[index].Grants, func(grant hostedMemberGrant) bool { return grant.ProjectID != string(project) })
	}
	return page, nil
}
