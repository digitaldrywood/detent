package operatortool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func EncodeWorkItemResult(value any) (Result, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Result{}, fmt.Errorf("encode work item result: %w", err)
	}
	if len(raw) <= MaxResultBytes {
		return Result{Content: raw}, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Result{}, fmt.Errorf("decode work item envelope: %w", err)
	}
	if envelope == nil {
		return Result{}, errors.New("work item envelope must be an object")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &item); err != nil {
		return Result{}, fmt.Errorf("decode work item content: %w", err)
	}
	if item == nil {
		return Result{}, errors.New("work item content must be an object")
	}
	var identity tracker.NativeReference
	if err := json.Unmarshal(envelope["data"], &identity); err != nil {
		return Result{}, fmt.Errorf("decode work item identity: %w", err)
	}
	truncated := []string{}
	if changed, err := truncateItemBody(item); err != nil {
		return Result{}, err
	} else if changed {
		truncated = append(truncated, "body")
	}
	if sourceRaw := item["linked_source"]; len(sourceRaw) > 0 && string(sourceRaw) != "null" {
		var source map[string]json.RawMessage
		if err := json.Unmarshal(sourceRaw, &source); err != nil {
			return Result{}, fmt.Errorf("decode linked source: %w", err)
		}
		if source == nil {
			return Result{}, errors.New("linked source must be an object")
		}
		if snapshotRaw := source["snapshot"]; len(snapshotRaw) > 0 && string(snapshotRaw) != "null" {
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(snapshotRaw, &snapshot); err != nil {
				return Result{}, fmt.Errorf("decode linked snapshot: %w", err)
			}
			if snapshot == nil {
				return Result{}, errors.New("linked snapshot must be an object")
			}
			if changed, err := truncateItemBody(snapshot); err != nil {
				return Result{}, err
			} else if changed {
				truncated = append(truncated, "linked_source.snapshot.body")
			}
			if len(snapshot["comments"]) > 4096 {
				snapshot["comments"] = json.RawMessage(`[]`)
				truncated = append(truncated, "linked_source.snapshot.comments")
			}
			source["snapshot"], err = json.Marshal(snapshot)
			if err != nil {
				return Result{}, fmt.Errorf("encode linked snapshot: %w", err)
			}
		}
		item["linked_source"], err = json.Marshal(source)
		if err != nil {
			return Result{}, fmt.Errorf("encode linked source: %w", err)
		}
	}
	item["content_read"], err = json.Marshal(struct {
		TruncatedFields []string        `json:"truncated_fields"`
		FullURL         string          `json:"full_url"`
		CommentsTool    string          `json:"comments_tool"`
		Arguments       WorkReadRequest `json:"arguments"`
	}{truncated, "/api/v2/organizations/" + url.PathEscape(string(identity.OrganizationID)) + "/projects/" + url.PathEscape(string(identity.ProjectID)) + "/work-items/" + url.PathEscape(string(identity.WorkItemID)), WorkComments, WorkReadRequest{ProjectID: string(identity.ProjectID), Reference: string(identity.WorkItemID), Limit: 1}})
	if err != nil {
		return Result{}, fmt.Errorf("encode work item continuation: %w", err)
	}
	envelope["data"], err = json.Marshal(item)
	if err != nil {
		return Result{}, fmt.Errorf("encode bounded work item: %w", err)
	}
	return encodeResult(envelope)
}

func truncateItemBody(fields map[string]json.RawMessage) (bool, error) {
	if fields == nil || len(fields["body"]) <= 4096 {
		return false, nil
	}
	var body string
	if err := json.Unmarshal(fields["body"], &body); err != nil {
		return false, fmt.Errorf("decode work item body: %w", err)
	}
	if len(body) <= 4096 {
		return false, nil
	}
	body = strings.ToValidUTF8(body[:4096], "")
	raw, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("encode work item body: %w", err)
	}
	fields["body"] = raw
	return true, nil
}
