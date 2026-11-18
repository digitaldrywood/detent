package recoveryfingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Tool(command string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return "", errors.New("agent backend command is empty")
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return Hash(command + ":" + err.Error()), err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Hash(path + ":" + err.Error()), err
	}
	return Hash(fmt.Sprintf(
		"command=%s;path=%s;size=%d;mode=%s;modified=%d",
		command,
		path,
		info.Size(),
		info.Mode(),
		info.ModTime().UnixNano(),
	)), nil
}
