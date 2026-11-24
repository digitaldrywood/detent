package attachmententry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type Response struct {
	header http.Header
	Body   bytes.Buffer
	Status int
}

func (r *Response) Header() http.Header            { return r.header }
func (r *Response) WriteHeader(status int)         { r.Status = status }
func (r *Response) Write(data []byte) (int, error) { return r.Body.Write(data) }

func (s *Service) HandleMCPResponse(c echo.Context, requestBody []byte, response *http.Response) error {
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
	if json.Unmarshal(requestBody, &call) != nil || call.Method != "tools/call" || !operatortool.IsAttachmentTool(call.Params.Name) && call.Params.Name != operatortool.ActionResult {
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
		Meta    json.RawMessage `json:"_meta"`
		IsError bool            `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame == nil || json.Unmarshal(frame["result"], &result) != nil || result.IsError || len(result.Content) != 1 {
		return nil
	}
	var authorized struct {
		EntryUpload     bool              `json:"entry_upload"`
		Mutation        mutation.Metadata `json:"mutation"`
		EntryAttachment string            `json:"entry_attachment"`
		Status          chat.ActionStatus `json:"status"`
		Preview         chat.Action       `json:"preview"`
		ObjectDeletion  string            `json:"object_deletion"`
	}
	if json.Unmarshal([]byte(result.Content[0].Text), &authorized) != nil {
		return nil
	}
	buffer := &Response{header: make(http.Header)}
	if call.Params.Name == operatortool.UploadAttachment && authorized.EntryUpload {
		buffer, err = s.UploadMCP(c.Request().Context(), c.Request(), c.Param("organization"), call.Params.Arguments, authorized.Mutation)
		if err != nil {
			return err
		}
	} else if (call.Params.Name == operatortool.ReadAttachment || call.Params.Name == operatortool.ReadAttachmentMetadata) && authorized.EntryAttachment == call.Params.Name {
		input, err := operatortool.DecodeAttachmentOperation(call.Params.Name, call.Params.Arguments)
		if err != nil {
			return nil
		}
		read := s.attachmentMCPContext(c, buffer, input)
		if call.Params.Name == operatortool.ReadAttachmentMetadata {
			err = s.readAttachmentMetadata(read)
		} else {
			err = s.readAttachmentContent(read, input)
		}
		if err != nil {
			return err
		}
	} else if (call.Params.Name == operatortool.DeleteAttachment || call.Params.Name == operatortool.ActionResult) && authorized.Preview.Kind == chat.ActionKind(operatortool.DeleteAttachment) && authorized.Status == chat.ActionSucceeded && authorized.ObjectDeletion == "pending" {
		input, err := operatortool.DecodeAttachmentOperation(operatortool.DeleteAttachment, authorized.Preview.Arguments)
		if err != nil {
			return nil
		}
		deletion := s.attachmentMCPContext(c, buffer, input)
		confirmed := s.finishAttachmentMCPDeletion(deletion, input)
		var receipt map[string]any
		if json.Unmarshal([]byte(result.Content[0].Text), &receipt) != nil || receipt == nil {
			return nil
		}
		if confirmed {
			receipt["object_deletion"] = "confirmed"
			if preview, ok := receipt["preview"].(map[string]any); ok {
				preview["result"] = "Attachment deleted; entry object deletion is confirmed."
			}
		}
		payload, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if _, err := buffer.Body.Write(payload); err != nil {
			return err
		}
		buffer.Status = http.StatusOK
	} else {
		return nil
	}
	payload := buffer.Body.Bytes()
	if len(payload) > operatortool.MaxResultBytes {
		payload = []byte(`{"code":"attachment_unavailable","message":"Attachment result exceeds the supported MCP size limit"}`)
		buffer.Status = http.StatusRequestEntityTooLarge
	}
	replacement := mcp.NewToolCallResult(c.Request().Header.Get("Mcp-Protocol-Version"), payload, buffer.Status != http.StatusCreated && buffer.Status != http.StatusOK)
	replacement.Meta = result.Meta
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

func (s *Service) UploadMCP(ctx context.Context, request *http.Request, organization string, arguments json.RawMessage, audit mutation.Metadata) (*Response, error) {
	input, content, err := operatortool.DecodeAttachmentArguments(arguments)
	if err != nil {
		return nil, err
	}
	request = request.Clone(mutation.WithContext(ctx, audit))
	request.URL.Path = "/organizations/" + organization + "/api/v2/projects/" + input.ProjectID + "/attachments"
	request.URL.RawPath, request.URL.RawQuery = "", ""
	request.Body = io.NopCloser(bytes.NewReader(content))
	request.ContentLength = int64(len(content))
	media := input.ContentType
	if media == "" {
		media = http.DetectContentType(content)
	}
	request.Header.Set("Content-Type", media)
	request.Header.Set("X-Attachment-Name", input.Name)
	request.Header.Set("Idempotency-Key", input.RequestID)
	buffer := &Response{header: make(http.Header)}
	s.Echo.ServeHTTP(buffer, request)
	return buffer, nil
}

func (s *Service) ReadMCP(ctx context.Context, request *http.Request, organization, name string, arguments json.RawMessage) (*Response, error) {
	input, err := operatortool.DecodeAttachmentOperation(name, arguments)
	if err != nil {
		return nil, err
	}
	request = request.Clone(ctx)
	request.Method = http.MethodGet
	request.URL.Path = "/organizations/" + organization + "/api/v2/projects/" + input.ProjectID + "/attachments/" + input.AttachmentID
	request.URL.RawPath, request.URL.RawQuery = "", ""
	request.Body, request.ContentLength = nil, 0
	if name == operatortool.ReadAttachmentMetadata {
		request.URL.Path += "/metadata"
	} else {
		request.URL.RawQuery = "offset=" + strconv.FormatInt(input.Offset, 10) + "&length=" + strconv.Itoa(input.Length)
	}
	buffer := &Response{header: make(http.Header)}
	s.Echo.ServeHTTP(buffer, request)
	return buffer, nil
}

func (s *Service) attachmentMCPContext(c echo.Context, buffer *Response, input operatortool.AttachmentOperationArguments) echo.Context {
	request := c.Request().Clone(c.Request().Context())
	request.Method = http.MethodGet
	request.URL.Path = "/organizations/" + c.Param("organization") + "/api/v2/projects/" + input.ProjectID + "/attachments/" + input.AttachmentID
	request.URL.RawPath, request.URL.RawQuery = "", ""
	request.Body, request.ContentLength = nil, 0
	read := s.Echo.NewContext(request, buffer)
	read.SetParamNames("organization", "project", "attachment")
	read.SetParamValues(c.Param("organization"), input.ProjectID, input.AttachmentID)
	return read
}

func (s *Service) readAttachmentContent(c echo.Context, input operatortool.AttachmentOperationArguments) error {
	if s.Storage() == nil {
		return NotFound(c)
	}
	organization, record, status, raw, err := s.attachmentMetadata(c)
	if err != nil || status != http.StatusOK {
		return CallError(c, status, raw, err, true)
	}
	if input.Offset > record.Size {
		return Error(c, http.StatusBadRequest, "invalid_request", "Offset exceeds the recorded attachment size")
	}
	body, err := s.openAttachment(c.Request().Context(), organization, record)
	if errors.Is(err, errAttachmentReadAudit) {
		return Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Attachment audit is unavailable")
	}
	if err != nil {
		return NotFound(c)
	}
	defer s.closeAttachmentBody(body, record.ID)
	if _, err := io.CopyN(io.Discard, body, input.Offset); err != nil {
		return Error(c, http.StatusBadGateway, "attachment_unavailable", "Attachment content is incomplete")
	}
	length := min(int64(input.Length), record.Size-input.Offset)
	content := make([]byte, int(length))
	if _, err := io.ReadFull(body, content); err != nil {
		return Error(c, http.StatusBadGateway, "attachment_unavailable", "Attachment content is incomplete")
	}
	record.AuthorizedPrincipal = ""
	c.Response().Header().Set("Cache-Control", "private, no-store")
	return c.JSON(http.StatusOK, operatortool.AttachmentContentResult{Metadata: record, ContentBase64: base64.StdEncoding.EncodeToString(content), Offset: input.Offset, ReturnedBytes: len(content), EOF: input.Offset+length == record.Size, MaxContentBytes: operatortool.AttachmentContentBytes})
}

func (s *Service) finishAttachmentMCPDeletion(c echo.Context, input operatortool.AttachmentOperationArguments) bool {
	unlock := s.Lock(c.Param("organization"), false)
	defer unlock()
	if s.Storage() == nil {
		return false
	}
	organization, err := s.Ready(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return false
	}
	status, raw, err := s.Call(c, organization, http.MethodPost, "/check", map[string]int64{"size": 0})
	if err != nil || status != http.StatusOK {
		return false
	}
	var authorized struct {
		Principal string `json:"principal_id"`
	}
	if json.Unmarshal(raw, &authorized) != nil || authorized.Principal == "" {
		return false
	}
	return s.removeAttachment(c.Request().Context(), organization, attachment.Metadata{ID: input.AttachmentID}, authorized.Principal) == nil
}
