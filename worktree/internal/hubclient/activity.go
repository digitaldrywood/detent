package hubclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *Client) Activity(ctx context.Context, organization tracker.OrganizationID, r operatortool.ActivityRequest) (operatortool.ActivityReport, error) {
	path, err := runnerOrganizationPath(organization)
	if err != nil {
		return operatortool.ActivityReport{}, err
	}
	params := url.Values{}
	for key, value := range map[string]string{"project_id": r.ProjectID, "runner_id": r.RunnerID, "from": r.From, "to": r.To} {
		if value != "" {
			params.Set(key, value)
		}
	}
	if r.Limit != 0 {
		params.Set("limit", strconv.Itoa(r.Limit))
	}
	var out operatortool.ActivityReport
	err = c.request(ctx, http.MethodGet, path+"/activity?"+params.Encode(), nil, &out)
	return out, err
}
