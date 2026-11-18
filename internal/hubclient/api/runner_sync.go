package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (n *NativeClient) SyncRunner(ctx context.Context, request runnerauth.SyncRequest) (runnerauth.SyncResponse, error) {
	c := n.client
	var result runnerauth.SyncResponse
	if !c.Enrolled() {
		return result, errors.New("runner sync requires an enrolled machine")
	}
	file, err := c.loadRunnerCredential()
	if err != nil {
		return result, err
	}
	if n.organization != file.Identity.OrganizationID {
		return result, errors.New("runner identity belongs to a different organization")
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
	if err == nil && result.Routing != nil {
		snapshot := result.Routing
		if result.Versions == nil || snapshot.RunnerID != file.Identity.RunnerID || snapshot.Revision != result.Versions.Routing || snapshot.Revision < 1 {
			return result, errors.New("unexpected routing identity in runner sync")
		}
		snapshot.Routing = snapshot.Routing.Normalized()
		if err := snapshot.Routing.Validate(); err != nil {
			return result, err
		}
		c.runner.routingMu.Lock()
		if previous := c.runner.routing; previous == nil {
			c.runner.routing = snapshot
			c.runner.notifyClaimChange()
		} else if snapshot.Revision >= previous.Revision {
			merged := *previous
			merged.Revision, merged.Routing, merged.RepositoryListRequest = snapshot.Revision, snapshot.Routing, snapshot.RepositoryListRequest
			c.runner.routing = &merged
			c.runner.notifyClaimChange()
		}
		c.runner.routingMu.Unlock()
	}
	if err == nil && request.Claim == nil && result.Versions != nil {
		c.runner.routingMu.Lock()
		c.runner.syncVersions = result.Versions
		c.runner.routingMu.Unlock()
	}
	return result, err
}

func (c *Client) runnerSyncVersions() *runnerauth.SyncVersions {
	if !c.Enrolled() {
		return nil
	}
	c.runner.routingMu.Lock()
	defer c.runner.routingMu.Unlock()
	return c.runner.syncVersions
}

func (n *NativeClient) RunnerSyncActive() bool { return n.client.runnerSyncVersions() != nil }
