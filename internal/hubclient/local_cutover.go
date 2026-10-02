package hubclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func (f *FleetClient) VerifyProjectCutover(ctx context.Context, local, repository, checkpoint string) error {
	id, ok := f.projects[local]
	if !ok || checkpoint == "" {
		return errors.New("selected project has no durable mapped cutover")
	}
	native, err := f.client.Native(f.organization, id)
	if err != nil {
		return err
	}
	project, err := native.Project(ctx)
	if err != nil {
		return err
	}
	if project.ID != id || project.OrganizationID != f.organization || project.Profile != "native" {
		return errors.New("mapped destination is not the selected native project")
	}
	var receipt struct {
		Checkpoint  string   `json:"checkpoint"`
		Applied     bool     `json:"applied"`
		Blockers    []string `json:"blockers"`
		Integration struct {
			Profile    string `json:"profile"`
			Repository string `json:"repository"`
			Intake     string `json:"intake"`
		} `json:"integration"`
	}
	if err := f.client.request(ctx, http.MethodGet, native.base()+"/integration/cutover", nil, &receipt); err != nil {
		return err
	}
	if !receipt.Applied || receipt.Checkpoint != checkpoint || len(receipt.Blockers) != 0 || receipt.Integration.Profile != "native" || receipt.Integration.Intake != "disabled" || repository == "" || !strings.EqualFold(receipt.Integration.Repository, repository) {
		return errors.New("selected durable cutover receipt does not match")
	}
	return nil
}
