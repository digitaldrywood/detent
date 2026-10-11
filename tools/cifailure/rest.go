package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cloudREST struct {
	base    string
	project string
	token   string
	client  *http.Client
}

func newCloudREST(raw, project, token string, client *http.Client) (*cloudREST, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("scheduled Cloud reporting requires the reviewed organization MCP URL")
	}
	path := strings.TrimPrefix(endpoint.Path, "/api/v2")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "organizations" || parts[1] == "" || parts[2] != "mcp" {
		return nil, errors.New("scheduled Cloud reporting requires an organization-scoped connection")
	}
	endpoint.Path = "/api/v2/organizations/" + parts[1] + "/projects/" + project
	endpoint.RawPath = ""
	return &cloudREST{base: endpoint.String(), project: project, token: token, client: client}, nil
}

func (c *cloudREST) request(ctx context.Context, method, path string, input, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return errors.New("scheduled REST request could not be encoded")
		}
	}
	for {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if err != nil {
			return errors.New("scheduled REST request could not be constructed")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		response, err := c.client.Do(req)
		failure := errors.New("scheduled REST transport unavailable; retained evidence requires retry")
		if err == nil {
			status := response.StatusCode
			if status >= 200 && status < 300 {
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(result)
				response.Body.Close()
				if decodeErr != nil {
					return errors.New("scheduled REST result could not be decoded")
				}
				return nil
			}
			response.Body.Close()
			failure = fmt.Errorf("scheduled REST request denied or unavailable (HTTP %d)", status)
			if status < 500 || status > 599 {
				return failure
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(failure, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *cloudREST) call(ctx context.Context, name string, args map[string]any, result any) error {
	if args["project_id"] != c.project {
		return errors.New("scheduled REST request belongs to another project")
	}
	var output any
	switch name {
	case "work_config":
		var project tracker.NativeProject
		if err := c.request(ctx, http.MethodGet, "", nil, &project); err != nil {
			return err
		}
		output = operatortool.WorkReadResult[map[string]any]{ProjectID: c.project, Data: map[string]any{"project": project}}
	case "work_list", "work_comments":
		query := url.Values{}
		for _, key := range []string{"open", "limit", "cursor", "query", "label", "fingerprint"} {
			if value, ok := args[key]; ok && fmt.Sprint(value) != "" {
				param := key
				if key == "query" {
					param = "q"
				}
				query.Set(param, fmt.Sprint(value))
			}
		}
		path := "/work-items"
		if name == "work_comments" {
			path += "/" + url.PathEscape(fmt.Sprint(args["reference"])) + "/comments"
		} else {
			query.Set("include", "summary")
		}
		var data json.RawMessage
		if err := c.request(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &data); err != nil {
			return err
		}
		output = operatortool.WorkReadResult[json.RawMessage]{ProjectID: c.project, Reference: stringArgument(args, "reference"), Data: data}
	case "work_item":
		var item tracker.NativeIssue
		if err := c.request(ctx, http.MethodGet, "/work-items/"+url.PathEscape(stringArgument(args, "reference")), nil, &item); err != nil {
			return err
		}
		output = operatortool.WorkReadResult[tracker.NativeIssue]{ProjectID: c.project, Reference: stringArgument(args, "reference"), Data: item}
	case "file_issue", "edit_item", "move_item", "add_comment":
		payload := map[string]any{"idempotency_key": args["request_id"]}
		for _, key := range []string{"title", "state", "labels", "priority", "expected_revision", "body"} {
			if value, ok := args[key]; ok {
				payload[key] = value
			}
		}
		path, method := "/work-items", http.MethodPost
		if name == "file_issue" {
			payload["body"] = args["description"]
			if priority, ok := args["priority"].(int); ok {
				payload["priority"] = priority - 1
			}
		} else {
			path += "/" + url.PathEscape(stringArgument(args, "identifier"))
			switch name {
			case "edit_item":
				method = http.MethodPatch
			case "move_item":
				path += "/workflow"
				payload["state"], payload["reason"] = args["target_state"], "worker_progress"
			case "add_comment":
				path += "/comments"
			}
		}
		if name == "add_comment" {
			return c.request(ctx, method, path, payload, result)
		}
		var item tracker.NativeIssue
		if err := c.request(ctx, method, path, payload, &item); err != nil {
			return err
		}
		if name == "move_item" {
			output = operatortool.WorkReadResult[tracker.NativeIssue]{ProjectID: c.project, Data: item}
		} else {
			output = map[string]any{"resource_id": item.WorkItemID, "revision": fmt.Sprint(item.Revision)}
		}
	default:
		return fmt.Errorf("scheduled REST operation %s is unsupported", name)
	}
	encoded, err := json.Marshal(output)
	if err != nil || json.Unmarshal(encoded, result) != nil {
		return errors.New("scheduled REST result could not be decoded")
	}
	return nil
}

func stringArgument(args map[string]any, key string) string {
	value, ok := args[key].(string)
	if !ok {
		return ""
	}
	return value
}
