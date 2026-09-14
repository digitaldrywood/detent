package cli

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

// hostedConversationFileConfig is the optional `conversation:` section of
// the hosted configuration file.
type hostedConversationFileConfig struct {
	Enabled bool `yaml:"enabled"`
	// Codex configures the coordinator backend for conversations without a
	// linked issue. When omitted, ordinary chat reports the coordinator as
	// unavailable and linked conversations still work.
	Codex *struct {
		Command         string                      `yaml:"command"`
		Model           string                      `yaml:"model"`
		ReasoningEffort string                      `yaml:"reasoning_effort"`
		Options         workflowconfig.CodexOptions `yaml:"options"`
	} `yaml:"codex"`
	// Workspace is the directory coordinator turns run in. Required when
	// codex is configured; it must exist.
	Workspace       string `yaml:"workspace"`
	QuestionTimeout string `yaml:"question_timeout"`
	// SettleWindow is how long a finished or idle conversation may sit
	// without activity before it settles (decisions section 14).
	SettleWindow     string `yaml:"settle_window"`
	ControlQueueSize int    `yaml:"control_queue_size"`
}

type codexBackendBuilder func(command string, cfg workflowconfig.CodexOptions) (runnerpkg.AgentBackend, error)

// readHostedConversationConfig reads the `conversation:` section of the
// hosted configuration file. enabled is false when the section is absent
// or disabled.
func readHostedConversationConfig(path string, buildCodex codexBackendBuilder) (config hubserver.ConversationConfig, enabled bool, resultErr error) {
	if strings.TrimSpace(path) == "" {
		return hubserver.ConversationConfig{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return hubserver.ConversationConfig{}, false, errors.New("hosted configuration could not be opened")
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("hosted configuration could not be closed"))
		}
	}()
	decoder := yaml.NewDecoder(io.LimitReader(file, 128*1024))
	decoder.KnownFields(true)
	var section hostedFileConfig
	if err := decoder.Decode(&section); err != nil {
		return hubserver.ConversationConfig{}, false, errors.New("hosted configuration is invalid")
	}
	return readConversationConfig(section.Conversation, buildCodex, os.Stat)
}

// readConversationConfig converts the file section into the hub's
// ConversationConfig. enabled is false for a nil or disabled section.
func readConversationConfig(section *hostedConversationFileConfig, buildCodex codexBackendBuilder, stat func(string) (os.FileInfo, error)) (hubserver.ConversationConfig, bool, error) {
	if section == nil || !section.Enabled {
		return hubserver.ConversationConfig{}, false, nil
	}
	if buildCodex == nil {
		buildCodex = buildCodexAgentBackend
	}
	if stat == nil {
		stat = os.Stat
	}
	config := hubserver.ConversationConfig{Enabled: true, ControlQueueSize: section.ControlQueueSize}
	if value := strings.TrimSpace(section.QuestionTimeout); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return hubserver.ConversationConfig{}, false, errors.New("conversation question_timeout must be a positive duration")
		}
		config.QuestionTimeout = timeout
	}
	if value := strings.TrimSpace(section.SettleWindow); value != "" {
		window, err := time.ParseDuration(value)
		if err != nil || window <= 0 {
			return hubserver.ConversationConfig{}, false, errors.New("conversation settle_window must be a positive duration")
		}
		config.SettleWindow = window
	}
	if section.Codex == nil {
		return config, true, nil
	}
	workspace := strings.TrimSpace(section.Workspace)
	if workspace == "" {
		return hubserver.ConversationConfig{}, false, errors.New("conversation workspace is required when codex is configured")
	}
	info, err := stat(workspace)
	if err != nil || !info.IsDir() {
		return hubserver.ConversationConfig{}, false, errors.New("conversation workspace must be an existing directory")
	}
	command := strings.TrimSpace(section.Codex.Command)
	if command == "" {
		command = "codex"
	}
	backend, err := buildCodex(command, section.Codex.Options)
	if err != nil {
		return hubserver.ConversationConfig{}, false, err
	}
	config.Backend = backend
	config.Workspace = workspace
	config.Model = strings.TrimSpace(section.Codex.Model)
	config.ReasoningEffort = strings.TrimSpace(section.Codex.ReasoningEffort)
	return config, true, nil
}
