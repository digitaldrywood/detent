package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

// hostedConversationFileConfig is the optional `conversation:` section of
// the hosted configuration file.
type hostedConversationFileConfig struct {
	Enabled bool `yaml:"enabled"`
	// Model is the model "auto" resolves to for this hub and the model
	// coordinator turns use.
	Model string `yaml:"model"`
	// ReasoningEffort is the effort coordinator turns use.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// Codex configures the coordinator backend for conversations without a
	// linked issue. When omitted, ordinary chat reports the coordinator as
	// unavailable and linked conversations still work.
	Codex *hostedConversationCodexFileConfig `yaml:"codex"`
	// Workspace is the directory coordinator turns run in. Required when
	// codex is configured; it must exist.
	Workspace       string `yaml:"workspace"`
	QuestionTimeout string `yaml:"question_timeout"`
	// SettleWindow is how long a finished or idle conversation may sit
	// without activity before it settles (decisions section 14).
	SettleWindow     string `yaml:"settle_window"`
	ControlQueueSize int    `yaml:"control_queue_size"`
}

// hostedConversationCodexFileConfig is the `conversation.codex:` section.
type hostedConversationCodexFileConfig struct {
	Command string                      `yaml:"command"`
	Options workflowconfig.CodexOptions `yaml:"options"`
}

// codexBackendBuilder builds the coordinator backend from the codex section
// for the absolute workspace.
type codexBackendBuilder func(command string, cfg workflowconfig.CodexOptions, workspace string) (runnerpkg.AgentBackend, error)

// readHostedConversationConfig reads the `conversation:` section of the
// hosted configuration file. enabled is false when the section is absent
// or disabled.
func readHostedConversationConfig(path string) (config hubserver.ConversationConfig, enabled bool, resultErr error) {
	return readHostedConversationConfigWithEnv(path, os.Getenv)
}

func readHostedConversationConfigWithEnv(path string, lookupEnv func(string) string) (config hubserver.ConversationConfig, enabled bool, resultErr error) {
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
	return readConversationConfig(section.Conversation, buildCoordinatorCodexBackend, os.Stat, lookupEnv)
}

// readConversationConfig converts the file section into the hub's
// ConversationConfig. enabled is false for a nil or disabled section.
func readConversationConfig(section *hostedConversationFileConfig, buildCodex codexBackendBuilder, stat func(string) (os.FileInfo, error), lookupEnv func(string) string) (hubserver.ConversationConfig, bool, error) {
	if section == nil || !section.Enabled {
		return hubserver.ConversationConfig{}, false, nil
	}
	if section.ControlQueueSize < 0 {
		return hubserver.ConversationConfig{}, false, errors.New("conversation control_queue_size must not be negative")
	}
	effort := strings.TrimSpace(section.ReasoningEffort)
	if effort != "" && !slices.Contains(conversation.ReasoningEfforts()[1:], effort) {
		return hubserver.ConversationConfig{}, false, fmt.Errorf("conversation reasoning_effort must be one of %s", strings.Join(conversation.ReasoningEfforts()[1:], ", "))
	}
	config := hubserver.ConversationConfig{
		Enabled: true, Model: strings.TrimSpace(section.Model), ReasoningEffort: effort,
		ControlQueueSize: section.ControlQueueSize,
	}
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
	if key := strings.TrimSpace(lookupEnv("OPENAI_API_KEY")); key != "" {
		backend, err := genkitbackend.NewProvider("openai", key)
		if err != nil {
			return hubserver.ConversationConfig{}, false, err
		}
		config.Backend = backend
		config.Model = genkitbackend.Model
		config.ReasoningEffort = "low"
		if section.Codex != nil {
			config.Workspace = strings.TrimSpace(section.Workspace)
		}
		modelBackend := sync.OnceValues(func() (hubserver.ConversationConfig, error) {
			if section.Codex == nil {
				return hubserver.ConversationConfig{}, errors.New("conversation coordinator model backend is unavailable")
			}
			modelConfig, _, err := readConversationConfig(section, buildCodex, stat, func(string) string { return "" })
			return modelConfig, err
		})
		config.ModelBackend = func() (runnerpkg.AgentBackend, string, error) {
			modelConfig, err := modelBackend()
			return modelConfig.Backend, modelConfig.Workspace, err
		}
		return config, true, nil
	}
	if section.Codex == nil {
		return config, true, nil
	}
	workspace := strings.TrimSpace(section.Workspace)
	if workspace == "" {
		return hubserver.ConversationConfig{}, false, errors.New("conversation workspace is required when codex is configured")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return hubserver.ConversationConfig{}, false, errors.New("conversation workspace must be an existing directory")
	}
	info, err := stat(workspace)
	if err != nil || !info.IsDir() {
		return hubserver.ConversationConfig{}, false, errors.New("conversation workspace must be an existing directory")
	}
	command := strings.TrimSpace(section.Codex.Command)
	if command == "" {
		command = defaultCoordinatorCodex
	}
	backend, err := buildCodex(command, section.Codex.Options, workspace)
	if err != nil {
		return hubserver.ConversationConfig{}, false, err
	}
	config.Backend = backend
	config.Workspace = workspace
	return config, true, nil
}
