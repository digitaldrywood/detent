package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func evidenceTool() runner.AgentTool {
	return runner.AgentTool{
		Name:        "attach_evidence",
		Description: "Attach an image or small text/log file from the current attempt workspace to this native issue, with a short caption. Use this for browser verification screenshots. The path must be workspace-relative; files outside the workspace and files over 20 MiB are refused. Evidence is recorded on the attempt and published with its completion comment. Never commit verification evidence to the repository.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"caption":{"type":"string","minLength":1,"maxLength":1024}},"required":["path","caption"],"additionalProperties":false}`),
	}
}

func (e *nativeExecution) SetEvidenceSource(source func(context.Context, string) (runner.ValidationEvidence, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evidenceSource = source
}

func (e *nativeExecution) attachEvidence(ctx context.Context, raw json.RawMessage) (runner.AgentToolResult, error) {
	result, err := e.uploadEvidence(ctx, raw)
	if err != nil {
		return runner.AgentToolResult{Content: err.Error()}, err
	}
	content, err := json.Marshal(result)
	return runner.AgentToolResult{Content: string(content), Success: err == nil}, err
}

func (e *nativeExecution) uploadEvidence(ctx context.Context, raw json.RawMessage) (attachment.Metadata, error) {
	var args struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return attachment.Metadata{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || strings.TrimSpace(args.Path) == "" || strings.TrimSpace(args.Caption) == "" || len(args.Caption) > 1024 || !utf8.ValidString(args.Caption) || strings.ContainsAny(args.Caption, "\r\n\x00") {
		return attachment.Metadata{}, attachment.ErrInvalid
	}
	e.mu.Lock()
	source, attempt, lease := e.evidenceSource, e.data.AttemptID, e.claim.lease
	e.mu.Unlock()
	if source == nil {
		return attachment.Metadata{}, errors.New("attempt evidence workspace is unavailable")
	}
	file, err := source(ctx, args.Path)
	if err != nil {
		return attachment.Metadata{}, err
	}
	if len(file.Content) > attachment.MaxBytes {
		return attachment.Metadata{}, attachment.ErrTooLarge
	}
	digest := sha256.New()
	if err := json.NewEncoder(digest).Encode([]string{attempt, file.Name, file.ContentType, args.Caption}); err != nil {
		return attachment.Metadata{}, err
	}
	if _, err := digest.Write(file.Content); err != nil {
		return attachment.Metadata{}, err
	}
	request := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: "evidence:" + hex.EncodeToString(digest.Sum(nil)), LeaseID: lease.ID, FencingToken: lease.FencingToken}, WorkItemID: lease.WorkItemID, AttemptID: attempt, Caption: args.Caption}
	encoded, err := json.Marshal(request)
	if err != nil {
		return attachment.Metadata{}, err
	}
	headers := http.Header{"Idempotency-Key": {request.IdempotencyKey}, "X-Detent-Evidence": {base64.RawURLEncoding.EncodeToString(encoded)}}
	result, err := e.claim.source.client.uploadAttachment(ctx, bytes.NewReader(file.Content), file.Name, file.ContentType, headers, "/attempts/"+url.PathEscape(attempt)+"/evidence")
	return result, nativeMutationError(err)
}
