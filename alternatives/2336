package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrProjectParticipants = errors.New("project lifecycle participants are unavailable")

func projectRecordActor(scope nativeScope) tenantstore.ProjectActor {
	actor := scope.actor()
	result := tenantstore.ProjectActor{CredentialID: scope.credential.ID, PrincipalID: actor.PrincipalID}
	if scope.credential.Hosted != nil {
		result.HostedSubject = scope.credential.Hosted.Subject
	}
	return result
}

func (s *Service) createHostedTenantProject(ctx context.Context, credential apiCredential, name string, states []tracker.NativeState) (string, error) {
	if states == nil {
		states = HostedProjectStates()
	}
	key, err := nativeCommandHash([]string{s.config.Hosted.OrganizationID, credential.Hosted.Subject, name})
	if err != nil {
		return "", err
	}
	request := createNativeProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Name: name, States: states}
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}
	raw, err := s.executeProjectRecordMutation(ctx, scope, nativeCommandOptions{OperationID: "create_hosted_project", Feature: "collaboration"}, request.Mutation, request)
	if err != nil {
		return "", err
	}
	var record tracker.NativeProject
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", err
	}
	return string(record.ID), nil
}
