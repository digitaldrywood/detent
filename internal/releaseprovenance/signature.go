package releaseprovenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return os.MkdirTemp(directory, "ro-")
}

func (signature *OverrideSignature) matches(manifest Manifest) bool {
	if signature == nil {
		return false
	}
	raw, err := Marshal(manifest)
	return err == nil && signature.manifestHash == sha256.Sum256(raw)
}

func VerifyOverrideTag(ctx context.Context, directory string, manifest Manifest) (*OverrideSignature, error) {
	if err := Validate(manifest, manifest.Repository, manifest.Tag, manifest.Commit); err != nil {
		return nil, err
	}
	if manifest.ValidationOverride == nil {
		return nil, errors.New("release provenance has no validation override")
	}
	reason, err := VerifyOverrideApproval(ctx, directory, manifest.Tag, manifest.Commit)
	if err != nil {
		return nil, err
	}
	if manifest.ValidationOverride.Reason != reason {
		return nil, errors.New("release override reason differs from signed approval")
	}
	raw, err := Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return &OverrideSignature{manifestHash: sha256.Sum256(raw)}, nil
}

func VerifyOverrideApproval(ctx context.Context, directory, version, commit string) (_ string, resultErr error) {
	if !regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(version) {
		return "", errors.New("override approval requires a stable release version")
	}
	expected, err := fullCommit(commit)
	if err != nil {
		return "", fmt.Errorf("override approval commit: %w", err)
	}
	git := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git")
		command.Args = append(command.Args, args...)
		command.Dir = directory
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("read override approval git object: %w", err)
		}
		return output, nil
	}
	object, err := git("rev-parse", "--verify", "refs/tags/release-override/"+version+"^{tag}")
	if err != nil {
		return "", err
	}
	raw, err := git("cat-file", "tag", strings.TrimSpace(string(object)))
	if err != nil {
		return "", err
	}
	header, body, ok := bytes.Cut(raw, []byte("\n\n"))
	if !ok {
		return "", errors.New("override approval has no tag message")
	}
	headers := strings.Split(string(header), "\n")
	if len(headers) < 3 || headers[0] != "object "+expected || headers[1] != "type commit" || headers[2] != "tag release-override/"+version {
		return "", errors.New("override approval does not name the exact release head and version")
	}
	message, signature, ok := bytes.Cut(body, []byte("-----BEGIN PGP SIGNATURE-----"))
	if !ok {
		return "", errors.New("override approval requires an OpenPGP signature")
	}
	reason := strings.TrimRight(string(message), "\n")
	if strings.TrimSpace(reason) == "" {
		return "", errors.New("release override requires a nonempty reason")
	}
	listing, err := git("show", expected+":.github/release-override-signers")
	if err != nil {
		return "", err
	}
	fingerprints := make(map[string]bool)
	fingerprintPattern := regexp.MustCompile(`^([A-Fa-f0-9]{40}|[A-Fa-f0-9]{64})$`)
	for line := range strings.SplitSeq(string(listing), "\n") {
		fingerprint := strings.TrimSpace(line)
		if fingerprint == "" {
			continue
		}
		if !fingerprintPattern.MatchString(fingerprint) {
			return "", errors.New("override signer list requires full OpenPGP fingerprints")
		}
		fingerprints[strings.ToUpper(fingerprint)] = true
	}
	if len(fingerprints) == 0 {
		return "", errors.New("override signer list is empty")
	}
	keys, err := git("show", expected+":.github/release-override-signers.asc")
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(keys), []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) || bytes.Contains(keys, []byte("PRIVATE KEY")) {
		return "", errors.New("override keyring requires armored public keys")
	}
	home, err := newGPGHome()
	if err != nil {
		return "", fmt.Errorf("create override verification keyring: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(home)) }()
	gpg := func(input []byte, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "gpg")
		command.Args = append(command.Args, "--no-options", "--batch", "--no-autostart", "--homedir", home, "--no-auto-key-retrieve")
		command.Args = append(command.Args, args...)
		command.Env = append(os.Environ(), "GNUPGHOME="+home)
		command.Stdin = bytes.NewReader(input)
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("verify override approval OpenPGP signature: %w", err)
		}
		return output, nil
	}
	if _, err := gpg(keys, "--import"); err != nil {
		return "", err
	}
	signaturePath := filepath.Join(home, "signature.asc")
	if err := os.WriteFile(signaturePath, append([]byte("-----BEGIN PGP SIGNATURE-----"), signature...), 0o600); err != nil {
		return "", fmt.Errorf("write override approval signature: %w", err)
	}
	signed := raw[:len(raw)-len(body)+len(message)]
	status, err := gpg(signed, "--status-fd=1", "--verify", signaturePath, "-")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 12 && fields[0] == "[GNUPG:]" && fields[1] == "VALIDSIG" && fingerprints[strings.ToUpper(fields[11])] {
			return reason, nil
		}
	}
	return "", errors.New("override approval signature is not from a listed primary key")
}
