package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	JSONRPCVersion          = "2.0"
	maxFrameDiagnosticBytes = 2 * 1024
	maxFrameIdentityBytes   = 256
)

var ErrInvalidFrame = errors.New("invalid json-rpc frame")

type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Codec struct {
	reader  *bufio.Reader
	writer  *bufio.Writer
	writeMu sync.Mutex
}

func NewCodec(r io.Reader, w io.Writer) *Codec {
	if r == nil {
		r = strings.NewReader("")
	}
	if w == nil {
		w = io.Discard
	}

	return &Codec{
		reader: bufio.NewReaderSize(r, 64*1024),
		writer: bufio.NewWriter(w),
	}
}

func (c *Codec) ReadMessage() (Message, error) {
	frame, err := c.reader.ReadBytes('\n')
	if len(frame) == 0 && errors.Is(err, io.EOF) {
		return Message{}, io.EOF
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return Message{}, fmt.Errorf("%w: read: %w", ErrInvalidFrame, err)
	}

	line := bytes.TrimSpace(frame)
	if len(line) == 0 {
		return Message{}, fmt.Errorf("%w: empty frame", ErrInvalidFrame)
	}

	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return Message{}, fmt.Errorf("%w: decode: %w: %s", ErrInvalidFrame, err, frameDiagnostic(line, msg))
	}
	if err := validateMessage(msg); err != nil {
		return Message{}, fmt.Errorf("%w: %s", err, frameDiagnostic(line, msg))
	}

	return msg, nil
}

func (c *Codec) drain() error {
	if _, err := io.Copy(io.Discard, c.reader); err != nil {
		return fmt.Errorf("drain json-rpc stream: %w", err)
	}
	return nil
}

func frameDiagnostic(frame []byte, msg Message) string {
	details := []string{fmt.Sprintf("frame_bytes=%d", len(frame))}
	if method := strings.TrimSpace(msg.Method); method != "" {
		excerpt, truncated := diagnosticExcerpt([]byte(method), maxFrameIdentityBytes)
		details = append(details, fmt.Sprintf("method=%q", excerpt))
		if truncated {
			details = append(details, "method_truncated=true")
		}
	}
	if id := bytes.TrimSpace(msg.ID); len(id) > 0 {
		excerpt, truncated := diagnosticExcerpt(id, maxFrameIdentityBytes)
		details = append(details, fmt.Sprintf("id=%s", excerpt))
		if truncated {
			details = append(details, "id_truncated=true")
		}
	}
	excerpt, truncated := diagnosticExcerpt(frame, maxFrameDiagnosticBytes)
	details = append(details, fmt.Sprintf("frame_excerpt=%q", excerpt))
	if truncated {
		details = append(details, "frame_truncated=true")
	}
	return strings.Join(details, " ")
}

func diagnosticExcerpt(value []byte, limit int) ([]byte, bool) {
	if len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}

func (c *Codec) WriteMessage(msg Message) error {
	if msg.JSONRPC == "" {
		msg.JSONRPC = JSONRPCVersion
	}
	if err := validateMessage(msg); err != nil {
		return err
	}

	frame, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal json-rpc message: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.writer.Write(frame); err != nil {
		return fmt.Errorf("write json-rpc frame: %w", err)
	}
	if err := c.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("write json-rpc frame newline: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush json-rpc frame: %w", err)
	}

	return nil
}

func validateMessage(msg Message) error {
	if msg.JSONRPC != "" && msg.JSONRPC != JSONRPCVersion {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidFrame, msg.JSONRPC)
	}

	hasMethod := msg.Method != ""
	hasResult := len(msg.Result) > 0
	hasError := msg.Error != nil

	switch {
	case hasResult && hasError:
		return fmt.Errorf("%w: response has result and error", ErrInvalidFrame)
	case hasMethod && (hasResult || hasError):
		return fmt.Errorf("%w: message cannot be both request and response", ErrInvalidFrame)
	case !hasMethod && !hasResult && !hasError:
		return fmt.Errorf("%w: missing method, result, or error", ErrInvalidFrame)
	case !hasMethod && len(msg.ID) == 0:
		return fmt.Errorf("%w: response missing id", ErrInvalidFrame)
	case hasError && strings.TrimSpace(msg.Error.Message) == "":
		return fmt.Errorf("%w: error response missing message", ErrInvalidFrame)
	default:
		return nil
	}
}
