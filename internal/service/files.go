package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writeDefinition(definition Definition) error {
	if err := os.MkdirAll(filepath.Dir(definition.Path), 0o755); err != nil {
		return fmt.Errorf("create service definition directory: %w", err)
	}
	file, err := os.OpenFile(definition.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create service definition: %w", err)
	}
	if _, err := file.WriteString(definition.Content); err != nil {
		return errors.Join(fmt.Errorf("write service definition: %w", err), file.Close(), os.Remove(definition.Path))
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("close service definition: %w", err), os.Remove(definition.Path))
	}
	return nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info != nil && info.Mode().IsRegular()
}
