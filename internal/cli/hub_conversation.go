package cli

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

// hostedConversationFileConfig is the optional `conversation:` section of
// the hosted configuration file.
type hostedConversationFileConfig struct {
	Enabled bool `yaml:"enabled"`
	// Model is the model "auto" resolves to for this hub.
	Model           string `yaml:"model"`
	QuestionTimeout string `yaml:"question_timeout"`
	// SettleWindow is how long a finished or idle conversation may sit
	// without activity before it settles (decisions section 14).
	SettleWindow     string `yaml:"settle_window"`
	ControlQueueSize int    `yaml:"control_queue_size"`
}

// readHostedConversationConfig reads the `conversation:` section of the
// hosted configuration file. enabled is false when the section is absent
// or disabled.
func readHostedConversationConfig(path string) (config hubserver.ConversationConfig, enabled bool, resultErr error) {
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
	return readConversationConfig(section.Conversation)
}

// readConversationConfig converts the file section into the hub's
// ConversationConfig. enabled is false for a nil or disabled section.
func readConversationConfig(section *hostedConversationFileConfig) (hubserver.ConversationConfig, bool, error) {
	if section == nil || !section.Enabled {
		return hubserver.ConversationConfig{}, false, nil
	}
	if section.ControlQueueSize < 0 {
		return hubserver.ConversationConfig{}, false, errors.New("conversation control_queue_size must not be negative")
	}
	config := hubserver.ConversationConfig{Enabled: true, Model: strings.TrimSpace(section.Model), ControlQueueSize: section.ControlQueueSize}
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
	return config, true, nil
}
