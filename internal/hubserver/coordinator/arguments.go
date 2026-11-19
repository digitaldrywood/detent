package coordinator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/runner"
)

var ErrToolArguments = errors.New("invalid tool arguments")

const (
	proposalTitleBytes = 500
	proposalBodyRunes  = 4000
)

func Tool(name, description, schema string) runner.AgentTool {
	return runner.AgentTool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func DecodeArguments(raw json.RawMessage, target any) error {
	return DecodeArgumentsWithLimit(raw, target, hubapi.CoordinatorToolArgumentBytes)
}

func DecodeArgumentsWithLimit(raw json.RawMessage, target any, limit int) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > limit {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrToolArguments, limit)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrToolArguments, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content", ErrToolArguments)
	}
	return nil
}

func ActionRequestID(messageID string, call runner.AgentToolCall) string {
	digest := sha256.Sum256([]byte(messageID + ":" + call.Name + ":" + string(call.Arguments)))
	return "luna_" + hex.EncodeToString(digest[:])
}

// EscapeData neutralizes the delimiters that fence data blocks so
// that quoted content cannot close its own block and read as instructions.
func EscapeData(value string) string {
	return coordinatorDataDelimiters.Replace(value)
}

var coordinatorDataDelimiters = strings.NewReplacer("<transcript>", "&lt;transcript&gt;", "</transcript>", "&lt;/transcript&gt;", "<issue_context>", "&lt;issue_context&gt;", "</issue_context>", "&lt;/issue_context&gt;", "<workflow_evidence>", "&lt;workflow_evidence&gt;", "</workflow_evidence>", "&lt;/workflow_evidence&gt;")

var ErrProjectUnreadable = errors.New("the conversation owner can no longer read this project")
