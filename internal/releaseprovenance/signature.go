package releaseprovenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

type OverrideSignature struct {
	manifestHash [sha256.Size]byte
}

func newGPGHome() (string, error) {
	directory := os.TempDir()
	if runtime.GOOS != "windows" {
		directory = "/tmp"
	}
	return os.MkdirTemp(directory, "release-gpg-")
}

func (signature *OverrideSignature) matches(manifest Manifest) bool {
	if signature == nil {
		return false
	}
	raw, err := Marshal(manifest)
	return err == nil && signature.manifestHash == sha256.Sum256(raw)
}

func VerifyOverrideTag(ctx context.Context, directory string, manifest Manifest, workflowActorsJSON string) (_ *OverrideSignature, resultErr error) {
	if err := Validate(manifest, manifest.Repository, manifest.Tag, manifest.Commit); err != nil {
		return nil, err
	}
	run := func(input string, environment []string, name string, args ...string) ([]byte, error) {
		var command *exec.Cmd
		switch name {
		case "git":
			command = exec.CommandContext(ctx, "git")
		case "gh":
			command = exec.CommandContext(ctx, "gh")
		case "gpg":
			command = exec.CommandContext(ctx, "gpg")
		default:
			return nil, errors.New("unsupported override signature command")
		}
		command.Args = append(command.Args, args...)
		command.Dir = directory
		command.Env = append(os.Environ(), environment...)
		command.Stdin = strings.NewReader(input)
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("run override signature %s: %w", name, err)
		}
		return output, nil
	}
	object, err := run("", nil, "git", "rev-parse", "--verify", "refs/tags/"+manifest.Tag+"^{tag}")
	if err != nil {
		return nil, err
	}
	objectID := strings.TrimSpace(string(object))
	commit, err := run("", nil, "git", "rev-parse", "--verify", objectID+"^{commit}")
	if err != nil {
		return nil, err
	}
	message, err := run("", nil, "git", "cat-file", "tag", objectID)
	if err != nil {
		return nil, err
	}
	actual, err := FromTagMessage(string(message), manifest.Repository, manifest.Tag, strings.TrimSpace(string(commit)))
	if err != nil {
		return nil, err
	}
	raw, err := Marshal(actual)
	if err != nil {
		return nil, err
	}
	signature := &OverrideSignature{manifestHash: sha256.Sum256(raw)}
	if !signature.matches(manifest) {
		return nil, errors.New("override signature does not cover the supplied release provenance")
	}
	actorsJSON := []byte(workflowActorsJSON)
	if workflowActorsJSON == "" {
		actorsJSON, err = run("", nil, "gh", "api", "repos/"+manifest.Repository+"/actions/variables/RELEASE_OVERRIDE_ACTORS", "--jq", ".value")
		if err != nil {
			return nil, err
		}
	}
	var actors []string
	if err := json.Unmarshal(actorsJSON, &actors); err != nil {
		return nil, fmt.Errorf("decode override owner allowlist: %w", err)
	}
	loginPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	for _, actor := range actors {
		if !loginPattern.MatchString(actor) {
			return nil, errors.New("invalid override owner allowlist")
		}
	}
	home, err := newGPGHome()
	if err != nil {
		return nil, fmt.Errorf("create override verification keyring: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(home)) }()
	environment := []string{"GNUPGHOME=" + home}
	keyIDs := make(map[string]bool)
	for _, actor := range actors {
		identityJSON, err := run("", nil, "gh", "api", "users/"+actor)
		if err != nil {
			return nil, err
		}
		var identity struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		}
		if err := json.Unmarshal(identityJSON, &identity); err != nil {
			return nil, fmt.Errorf("decode override owner identity: %w", err)
		}
		if identity.Type != "User" || !strings.EqualFold(identity.Login, actor) {
			continue
		}
		keysJSON, err := run("", nil, "gh", "api", "--paginate", "--slurp", "users/"+actor+"/gpg_keys?per_page=100")
		if err != nil {
			return nil, err
		}
		var pages [][]struct {
			KeyID   string `json:"key_id"`
			RawKey  string `json:"raw_key"`
			Revoked bool   `json:"revoked"`
		}
		if err := json.Unmarshal(keysJSON, &pages); err != nil {
			return nil, fmt.Errorf("decode override owner signing keys: %w", err)
		}
		for _, page := range pages {
			for _, key := range page {
				if len(key.KeyID) != 16 || key.RawKey == "" || key.Revoked {
					continue
				}
				if _, err := run(key.RawKey, environment, "gpg", "--no-options", "--batch", "--no-autostart", "--homedir", home, "--import"); err != nil {
					return nil, err
				}
				keyIDs[strings.ToUpper(key.KeyID)] = true
			}
		}
	}
	if len(keyIDs) == 0 {
		return nil, errors.New("override tag has no allowlisted owner signing keys")
	}
	command := exec.CommandContext(ctx, "git")
	command.Args = append(command.Args, "-c", "gpg.format=openpgp", "-c", "gpg.openpgp.program=gpg", "-c", "gpg.program=gpg", "verify-tag", "--raw", objectID)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	var status bytes.Buffer
	command.Stderr = &status
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("verify override tag signature: %w", err)
	}
	for line := range strings.SplitSeq(status.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 12 || fields[0] != "[GNUPG:]" || fields[1] != "VALIDSIG" {
			continue
		}
		fingerprint := fields[len(fields)-1]
		if len(fingerprint) >= 40 && keyIDs[strings.ToUpper(fingerprint[len(fingerprint)-16:])] {
			return signature, nil
		}
	}
	return nil, errors.New("override tag signature is not from an allowlisted owner key")
}
