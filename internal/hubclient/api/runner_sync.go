package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (c *Client) SyncRunner(ctx context.Context, request runnerauth.SyncRequest) (runnerauth.SyncResponse, error) {
	var result runnerauth.SyncResponse
	if !c.Enrolled() {
		return result, errors.New("runner sync requires an enrolled machine")
	}
	file, err := c.loadRunnerCredential()
	if err != nil {
		return result, err
	}
	base, err := RunnerOrganizationPath(file.Identity.OrganizationID)
	if err != nil {
		return result, err
	}
	request.MachineID = file.Identity.MachineID
	path := base + "/runners/" + file.Identity.RunnerID + "/sync"
	err = c.RequestWithDeadline(ctx, time.Duration(request.WaitSeconds)*time.Second+10*time.Second, http.MethodPost, path, nil, request, &result)
	if err == nil && !result.NoChange && result.Versions == nil {
		return result, errors.New("runner sync response is missing versions")
	}
	return result, err
}
