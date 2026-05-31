package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	JSONRPCVersion   = "2.0"
	MaxScanTokenSize = 2 * 1024 * 1024
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
	scanner *bufio.Scanner
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

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxScanTokenSize)

	return &Codec{
		scanner: scanner,
		writer:  bufio.NewWriter(w),
	}
}

func (c *Codec) ReadMessage() (Message, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Message{}, fmt.Errorf("%w: scan: %w", ErrInvalidFrame, err)
		}
		return Message{}, io.EOF
	}

	line := strings.TrimSpace(string(c.scanner.Bytes()))
	if line == "" {
		return Message{}, fmt.Errorf("%w: empty frame", ErrInvalidFrame)
	}

	var msg Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return Message{}, fmt.Errorf("%w: decode: %w", ErrInvalidFrame, err)
	}
	if err := validateMessage(msg); err != nil {
		return Message{}, err
	}

	return msg, nil
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
