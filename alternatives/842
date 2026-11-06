package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func ProviderName(backend AgentBackend) string {
	if backend.Provider != "" {
		return backend.Provider
	}
	switch backend.Kind {
	case AgentBackendCodex:
		if provider := backend.CodexOptions().ModelProvider; provider != "" {
			return provider
		}
		return "openai"
	case AgentBackendClaudeCode:
		return "anthropic"
	default:
		return backend.Kind
	}
}

func ProviderIdentity(backend AgentBackend) string {
	identity := []any{backend.Kind, backend.Protocol, ProviderCommandIdentity(backend), ProviderName(backend)}
	switch backend.Kind {
	case AgentBackendCodex:
		options := backend.CodexOptions()
		identity = append(identity, options.Shell, options.ModelProvider)
	case AgentBackendClaudeCode:
		options := backend.ClaudeCodeOptions()
		identity = append(identity, options.Shell, options.ExtraArgs)
	default:
		identity = append(identity, backend)
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		panic(fmt.Errorf("encode provider identity: %w", err))
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func ProviderCompatible(a, b AgentBackend) bool { return ProviderIdentity(a) == ProviderIdentity(b) }

func ProviderCommandIdentity(backend AgentBackend) string {
	if backend.Kind != AgentBackendCodex {
		return backend.Command
	}
	args := strings.FieldsFunc(backend.Command, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(args) > 0 && (args[0] == "env" || args[0] == "/usr/bin/env") {
		args = args[1:]
		for len(args) > 0 && strings.Contains(args[0], "=") {
			name, value, _ := strings.Cut(args[0], "=")
			if name == "" || strings.IndexFunc(name, func(r rune) bool {
				return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
			}) >= 0 || name[0] >= '0' && name[0] <= '9' {
				return backend.Command
			}
			if strings.HasPrefix(name, "CODEX_") || name == "HOME" || name == "PATH" || strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "AZURE_") || strings.HasPrefix(name, "XDG_") || strings.ContainsAny(value, "\"'`$;|&<>\n") {
				return backend.Command
			}
			args = args[1:]
		}
	}
	if len(args) < 2 || filepath.Base(args[0]) != "codex" || args[len(args)-1] != "app-server" {
		return backend.Command
	}
	for args = args[1 : len(args)-1]; len(args) > 0; args = args[2:] {
		if len(args) < 2 || args[0] != "--config" && args[0] != "-c" {
			return backend.Command
		}
		switch args[1] {
		case "shell_environment_policy.inherit=all", "model_reasoning_effort=none", "model_reasoning_effort=minimal", "model_reasoning_effort=low", "model_reasoning_effort=medium", "model_reasoning_effort=high", "model_reasoning_effort=xhigh", "model_reasoning_effort=max":
		default:
			return backend.Command
		}
	}
	return "codex app-server"
}
