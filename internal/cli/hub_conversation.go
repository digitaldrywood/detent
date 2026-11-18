package cli

import (
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

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
	return section.Conversation.Build(buildCoordinatorCodexBackend, os.Stat, lookupEnv)
}
