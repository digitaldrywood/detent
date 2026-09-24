package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/instancelock"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

// ApplyRunningUpdate delegates binary replacement to the instance holding this
// configuration's lock. A live instance must never fall back to offline apply.
func ApplyRunningUpdate(ctx context.Context, cmd *cobra.Command, apply detentupdate.ApplyOptions) (detentupdate.Status, bool, error) {
	configPath, err := cmd.Flags().GetString("config")
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	opts := defaultOptions()
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	inspection, err := instancelock.Inspect(filepath.Join(filepath.Dir(resolution.Path), "detent.db.lock"))
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	if inspection.Status != instancelock.StatusHeld {
		return detentupdate.Status{}, false, nil
	}
	if !apply.AssumeYes && !apply.FromRelease {
		return detentupdate.Status{}, true, errors.New("a running Detent instance must apply its own update; use detent update --yes to drain active attempts and restart")
	}
	host, err := cmd.Flags().GetString("host")
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	client, err := newDashboardReadClient(ctx, resolution.Path, host, port, flagChanged(cmd, "port"), opts)
	if err != nil {
		return detentupdate.Status{}, true, err
	}
	status, err := client.applyRunningUpdate(ctx, apply)
	return status, true, err
}

func (c *DashboardReadClient) applyRunningUpdate(ctx context.Context, apply detentupdate.ApplyOptions) (detentupdate.Status, error) {
	state, err := c.updateState(ctx)
	if err != nil {
		return detentupdate.Status{}, fmt.Errorf("check running Detent update coordination: %w", err)
	}
	if state.Update.ActiveAttempts == nil {
		return detentupdate.Status{}, errors.New("running Detent does not support coordinated update draining")
	}

	payload, err := json.Marshal(struct {
		Confirm     bool `json:"confirm"`
		Release     bool `json:"release"`
		FromRelease bool `json:"from_release"`
	}{true, true, apply.FromRelease})
	if err != nil {
		return detentupdate.Status{}, err
	}
	target := *c.baseURL
	target.Path = "/api/v1/update/apply"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return detentupdate.Status{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}
	stopProgress := c.reportDrain(ctx, apply.Stderr, "update")
	defer stopProgress()
	response, err := c.http.Do(request)
	if err != nil {
		return detentupdate.Status{}, fmt.Errorf("apply update through running instance: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return detentupdate.Status{}, decodeDashboardResponseError(response)
	}
	var status detentupdate.Status
	err = decodeDashboardJSON(response.Body, &status)
	return status, err
}

type runningUpdateState struct {
	Update struct {
		ActiveAttempts *int `json:"active_attempts"`
	} `json:"update"`
	Counts struct {
		Running int `json:"running"`
	} `json:"counts"`
}

func (c *DashboardReadClient) updateState(ctx context.Context) (runningUpdateState, error) {
	if c == nil || c.baseURL == nil {
		return runningUpdateState{}, errors.New("dashboard API client is not configured")
	}
	target := *c.baseURL
	target.Path = "/api/v1/state"
	target.RawPath = ""
	target.RawQuery = url.Values{"fields": {"update,counts"}}.Encode()
	var state runningUpdateState
	_, err := c.readJSON(ctx, target, &state)
	return state, err
}

func (c *DashboardReadClient) reportDrain(ctx context.Context, out io.Writer, reason string) func() {
	progressCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			state, err := c.updateState(progressCtx)
			if err == nil && state.Update.ActiveAttempts != nil && out != nil {
				fmt.Fprintf(out, "draining for %s: %d active attempts\n", reason, state.Counts.Running)
			}
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
