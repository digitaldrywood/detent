package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	ProtocolVersion        = "2026-07-28"
	LegacyProtocolVersion  = "2025-11-25"
	codeUnsupportedVersion = -32022
	codeHeaderMismatch     = -32020
	protocolMetaKey        = "io.modelcontextprotocol/protocolVersion"
	capabilitiesMetaKey    = "io.modelcontextprotocol/clientCapabilities"
	clientMetaKey          = "io.modelcontextprotocol/clientInfo"
)

func supportedVersions() []string {
	return []string{ProtocolVersion, LegacyProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"}
}

type requestMetadata struct {
	Version string
	Client  string
	Modern  bool
}

// Metadata describes the protocol only. Identity, grants, application handles
// and confirmation mode come exclusively from the authenticated application.
func metadata(message request) (requestMetadata, *rpcError) {
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := decodeObject(message.Params, &params, false); err != nil {
		return requestMetadata{}, &rpcError{Code: codeInvalidParams, Message: "Invalid request parameters"}
	}
	raw, present := params.Meta[protocolMetaKey]
	if !present {
		return requestMetadata{}, nil
	}
	meta := requestMetadata{Modern: true}
	if json.Unmarshal(raw, &meta.Version) != nil || meta.Version == "" || len(meta.Version) > 32 {
		return meta, &rpcError{Code: codeInvalidParams, Message: "Invalid protocol metadata"}
	}
	if meta.Version != ProtocolVersion {
		return meta, &rpcError{Code: codeUnsupportedVersion, Message: "Unsupported protocol version", Data: map[string]any{"supported": supportedVersions(), "requested": meta.Version}}
	}
	if !isJSONObject(params.Meta[capabilitiesMetaKey]) {
		return meta, &rpcError{Code: codeInvalidParams, Message: "Invalid protocol metadata"}
	}
	if raw, present := params.Meta[clientMetaKey]; present {
		var info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if !isJSONObject(raw) || json.Unmarshal(raw, &info) != nil || strings.TrimSpace(info.Name) == "" || strings.TrimSpace(info.Version) == "" || len(info.Name) > 256 || len(info.Version) > 256 {
			return meta, &rpcError{Code: codeInvalidParams, Message: "Invalid client metadata"}
		}
		meta.Client = info.Name
	}
	return meta, nil
}

func (s *session) openConnection(ctx context.Context, client string) error {
	if opener, ok := s.executor.(interface{ OpenConnection(context.Context) error }); ok {
		connection := operatortool.CurrentConnection(ctx)
		return opener.OpenConnection(operatortool.BindConnection(ctx, connection.ID, client))
	}
	return nil
}

func (s *session) serverInfo() map[string]any {
	return map[string]any{"name": serverName, "title": serverTitle, "version": s.version}
}

func (s *session) writeVersionResult(id json.RawMessage, version, method string, result any) error {
	if version != ProtocolVersion {
		return s.writeResult(id, result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return s.writeError(id, codeInternalError, "Internal error", nil)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return s.writeError(id, codeInternalError, "Internal error", nil)
	}
	object["resultType"] = json.RawMessage(`"complete"`)
	switch method {
	case "server/discover", "tools/list":
		object["ttlMs"] = json.RawMessage(`0`)
		object["cacheScope"] = json.RawMessage(`"private"`)
	}
	info, err := json.Marshal(map[string]any{"io.modelcontextprotocol/serverInfo": s.serverInfo()})
	if err != nil {
		return s.writeError(id, codeInternalError, "Internal error", nil)
	}
	object["_meta"] = info
	return s.writeResult(id, object)
}
