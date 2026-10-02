package cloudentry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

type attachmentResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *attachmentResponse) Header() http.Header            { return r.header }
func (r *attachmentResponse) WriteHeader(status int)         { r.status = status }
func (r *attachmentResponse) Write(data []byte) (int, error) { return r.body.Write(data) }

func (s *Service) attachmentMCPResponse(c echo.Context, requestBody []byte, response *http.Response) error {
	if c.Request().Method != http.MethodPost || !strings.HasSuffix(c.Request().URL.Path, "/mcp") || response.StatusCode != http.StatusOK {
		return nil
	}
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(requestBody, &call) != nil || call.Method != "tools/call" || call.Params.Name != operatortool.UploadAttachment {
		return nil
	}
	input, content, err := operatortool.DecodeAttachmentArguments(call.Params.Arguments)
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	var frame map[string]json.RawMessage
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &frame) != nil || json.Unmarshal(frame["result"], &result) != nil || result.IsError || len(result.Content) != 1 {
		return nil
	}
	var authorized struct {
		EntryUpload bool `json:"entry_upload"`
	}
	if json.Unmarshal([]byte(result.Content[0].Text), &authorized) != nil || !authorized.EntryUpload {
		return nil
	}
	request := c.Request().Clone(c.Request().Context())
	request.URL.Path = "/organizations/" + c.Param("organization") + "/api/v2/projects/" + input.ProjectID + "/attachments"
	request.URL.RawPath, request.URL.RawQuery = "", ""
	request.Body = io.NopCloser(bytes.NewReader(content))
	request.ContentLength = int64(len(content))
	media := input.ContentType
	if media == "" {
		media = http.DetectContentType(content)
	}
	request.Header.Set("Content-Type", media)
	request.Header.Set("X-Attachment-Name", input.Name)
	buffer := &attachmentResponse{header: make(http.Header)}
	upload := s.echo.NewContext(request, buffer)
	upload.SetParamNames("organization", "project")
	upload.SetParamValues(c.Param("organization"), input.ProjectID)
	if err := s.uploadAttachment(upload); err != nil {
		return err
	}
	payload := buffer.body.Bytes()
	replacement := map[string]any{"content": []map[string]string{{"type": "text", "text": string(payload)}}}
	if buffer.status != http.StatusCreated {
		replacement["isError"] = true
	} else if c.Request().Header.Get("Mcp-Protocol-Version") != "2024-11-05" && c.Request().Header.Get("Mcp-Protocol-Version") != "2025-03-26" {
		replacement["structuredContent"] = json.RawMessage(payload)
	}
	frame["result"], err = json.Marshal(replacement)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(frame)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	response.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return nil
}
