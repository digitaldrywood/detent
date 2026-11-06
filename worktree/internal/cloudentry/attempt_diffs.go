package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func diffProject(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) && safeID(parts[i+1]) {
			return parts[i+1]
		}
	}
	return ""
}

func diffUploadRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/attempts/") && strings.HasSuffix(r.URL.Path, "/diff")
}

func (s *Service) storeDiffBody(ctx context.Context, organization, project string, ref diffbody.Reference, patch string) error {
	if s.attachments == nil {
		return errors.New("diff object storage unavailable")
	}
	if err := ref.Validate(organization, project, tracker.MaxDiffPatchBytes); err != nil {
		return err
	}
	if int64(len(patch)) != ref.Bytes || diffbody.Digest(patch) != ref.SHA256 {
		return errors.New("diff body integrity mismatch")
	}
	putErr := s.attachments.Put(ctx, ref.Key, strings.NewReader(patch), ref.Bytes, "text/plain; charset=utf-8", ref.SHA256)
	stored, err := s.readDiffBody(ctx, organization, project, ref)
	if err != nil {
		return errors.Join(putErr, err)
	}
	if stored != patch {
		return errors.New("stored diff body integrity mismatch")
	}
	return nil
}

func (s *Service) readDiffBody(ctx context.Context, organization, project string, ref diffbody.Reference) (string, error) {
	if s.attachments == nil {
		return "", errors.New("diff object storage unavailable")
	}
	if err := ref.Validate(organization, project, tracker.MaxDiffPatchBytes); err != nil {
		return "", err
	}
	body, err := s.attachments.Open(ctx, ref.Key)
	if err != nil {
		return "", err
	}
	content, err := io.ReadAll(io.LimitReader(body, ref.Bytes+1))
	err = errors.Join(err, body.Close())
	if err != nil {
		return "", err
	}
	patch := string(content)
	if int64(len(content)) != ref.Bytes || diffbody.Digest(patch) != ref.SHA256 {
		return "", errors.New("diff body integrity mismatch")
	}
	return patch, nil
}

func (s *Service) uploadAttemptDiff(c echo.Context, organization Organization, claims cloudassert.Claims, raw []byte) ([]byte, int, []byte, error) {
	var request tracker.AttemptDiffRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, http.StatusUnprocessableEntity, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, http.StatusUnprocessableEntity, nil, errors.New("invalid diff JSON")
	}
	for _, file := range request.Files {
		if file.PatchBody != nil || file.PatchExpired {
			return nil, http.StatusUnprocessableEntity, nil, errors.New("diff body references are entry-owned")
		}
	}
	request.PostedBytes = 0
	for _, file := range request.Files {
		request.PostedBytes += int64(len(file.Patch))
	}
	files, total := tracker.NormalizeDiffFiles(request.Files)
	if total > tracker.MaxDiffBytes {
		return nil, http.StatusRequestEntityTooLarge, []byte(`{"code":"diff_too_large","message":"Diff patches exceed the stored diff limit"}`), nil
	}
	if err := tracker.ValidateDiffFiles(files); err != nil {
		return nil, http.StatusUnprocessableEntity, nil, err
	}
	patches := make([]string, len(files))
	diffID := "diff_" + strings.TrimPrefix(conversation.NewAttachmentID(), "att_")
	for i := range files {
		patches[i] = files[i].Patch
		if files[i].Patch != "" {
			ref := diffbody.New(organization.ID, diffProject(c.Request().URL.Path), diffID, i, files[i].Patch)
			files[i].PatchBody = &ref
			files[i].Patch = ""
		}
	}
	request.Files = files
	body, err := json.Marshal(request)
	if err != nil {
		return nil, 0, nil, err
	}
	if len(body) > cloudassert.MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, nil, errors.New("diff metadata exceeds the shared entry limit")
	}
	check := claims
	check.Path = c.Request().URL.Path + "/check"
	check.BodyDigest = cloudassert.BodyDigest(body)
	check.ID, err = cloudassert.NewID()
	if err != nil {
		return nil, 0, nil, err
	}
	bearer := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	status, result, err := s.signedCallCSRF(c.Request().Context(), organization, check, body, bearer, c.Request().Header.Get("X-CSRF-Token"))
	if err != nil || status != http.StatusNoContent {
		return nil, status, result, err
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	for i, file := range files {
		if file.PatchBody == nil {
			continue
		}
		if err := s.storeDiffBody(c.Request().Context(), organization.ID, diffProject(c.Request().URL.Path), *file.PatchBody, patches[i]); err != nil {
			return nil, http.StatusBadGateway, nil, err
		}
	}
	return body, 0, nil, nil
}

func (s *Service) hydrateAttemptDiff(ctx context.Context, organization, project string, diff *tracker.AttemptDiff) error {
	if diff == nil {
		return nil
	}
	for i := range diff.Files {
		file := &diff.Files[i]
		if file.PatchBody == nil {
			continue
		}
		patch, err := s.readDiffBody(ctx, organization, project, *file.PatchBody)
		if err != nil {
			return err
		}
		file.Patch = patch
		file.PatchBody = nil
	}
	return nil
}

func replaceDiffResponse(response *http.Response, raw []byte) {
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	response.Header.Set("Content-Length", strconv.Itoa(len(raw)))
}

func (s *Service) attemptDiffResponse(c echo.Context, organization Organization, requestBody []byte, response *http.Response) error {
	if response.StatusCode != http.StatusOK {
		return nil
	}
	isRead := c.Request().Method == http.MethodGet && strings.HasSuffix(c.Request().URL.Path, "/diff")
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name      string                       `json:"name"`
			Arguments operatortool.ChangeArguments `json:"arguments"`
		} `json:"params"`
	}
	isMCP := c.Request().Method == http.MethodPost && strings.HasSuffix(c.Request().URL.Path, "/mcp") && json.Unmarshal(requestBody, &call) == nil && call.Method == "tools/call" && (call.Params.Name == operatortool.GetAttemptDiff || call.Params.Name == operatortool.GetWorkItemDiff)
	if !isRead && !isMCP {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 6*tracker.MaxDiffBytes+(8<<20)))
	if err = errors.Join(err, response.Body.Close()); err != nil {
		return err
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	if isMCP {
		var frame map[string]json.RawMessage
		var result struct {
			Meta    json.RawMessage `json:"_meta"`
			IsError bool            `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			return err
		}
		if frame == nil || len(frame["result"]) == 0 {
			replaceDiffResponse(response, raw)
			return nil
		}
		if err := json.Unmarshal(frame["result"], &result); err != nil {
			return err
		}
		if result.IsError || len(result.Content) != 1 {
			replaceDiffResponse(response, raw)
			return nil
		}
		var value operatortool.ChangeResult
		if err := json.Unmarshal([]byte(result.Content[0].Text), &value); err != nil {
			return err
		}
		if err := s.hydrateAttemptDiff(c.Request().Context(), organization.ID, call.Params.Arguments.ProjectID, value.Diff); err != nil {
			return err
		}
		args := call.Params.Arguments
		value.Diff, value.NextOffset = pageHydratedDiff(value.Diff, args, value.NextOffset)
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		replacement := mcp.NewToolCallResult(c.Request().Header.Get("Mcp-Protocol-Version"), payload, false)
		replacement.Meta = result.Meta
		frame["result"], err = json.Marshal(replacement)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(frame)
		if err != nil {
			return err
		}
	} else if strings.Contains(c.Request().URL.Path, "/attempts/") {
		var diff tracker.AttemptDiff
		if err := json.Unmarshal(raw, &diff); err != nil {
			return err
		}
		if err := s.hydrateAttemptDiff(c.Request().Context(), organization.ID, diffProject(c.Request().URL.Path), &diff); err != nil {
			return err
		}
		raw, err = json.Marshal(diff)
		if err != nil {
			return err
		}
	} else {
		var value tracker.WorkItemDiff
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if err := s.hydrateAttemptDiff(c.Request().Context(), organization.ID, diffProject(c.Request().URL.Path), value.Diff); err != nil {
			return err
		}
		raw, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	replaceDiffResponse(response, raw)
	return nil
}

func pageHydratedDiff(diff *tracker.AttemptDiff, args operatortool.ChangeArguments, next *int) (*tracker.AttemptDiff, *int) {
	offset := args.Offset
	args.Offset = 0
	value, trimmed := operatortool.ChangeDiffPage(diff, args)
	if trimmed != nil {
		position := offset + *trimmed
		next = &position
	}
	return value, next
}
