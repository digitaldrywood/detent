package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
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
	state, err := c.State(ctx, "")
	if err != nil {
		return detentupdate.Status{}, err
	}
	update, ok := state.field("update").(map[string]any)
	if !ok {
		return detentupdate.Status{}, errors.New("running Detent does not support coordinated update draining")
	}
	if _, ok := update["active_attempts"]; !ok {
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

func (c *DashboardReadClient) reportDrain(ctx context.Context, out io.Writer, reason string) func() {
	progressCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			state, err := c.State(progressCtx, "")
			if err == nil && out != nil {
				if running, ok := state.field("running").([]any); ok {
					count := strconv.Itoa(len(running))
					if counts, ok := state.field("counts").(map[string]any); ok {
						if total, ok := counts["running"].(json.Number); ok {
							count = total.String()
						}
					}
					fmt.Fprintf(out, "draining for %s: %s active attempts\n", reason, count)
				}
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
