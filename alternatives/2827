package releaseaudit

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const BundleCommit = "c1c8a8c1471069fb0e188eeaff69b8e8db6564a8"
const BundleDigest = "9c550a639c12bacbb1e59b5893c7d86cd03145205b769ac5ab2c45ed7c794c73"

//go:embed bundle/*
var bundle embed.FS

func ReadInstruction(name string) ([]byte, error) {
	if strings.ContainsAny(name, "/\\") || filepath.Ext(name) != ".md" && name != "report-schema.json" {
		return nil, errors.New("release audit instruction is outside the trusted bundle")
	}
	return bundle.ReadFile("bundle/" + name)
}

func prepareBundle(directory string) error {
	entries, err := fs.ReadDir(bundle, "bundle")
	if err != nil {
		return err
	}
	hash := sha256.New()
	for _, entry := range entries {
		content, err := bundle.ReadFile("bundle/" + entry.Name())
		if err != nil {
			return err
		}
		hash.Write([]byte(entry.Name() + "\x00"))
		hash.Write(content)
		hash.Write([]byte{0})
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), content, 0o400); err != nil {
			return err
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != BundleDigest {
		return errors.New("release audit bundle digest differs from the trusted pin")
	}
	return nil
}

func validateDocument(ctx context.Context, directory, kind string, content []byte) error {
	if len(content) > maxDocumentBytes {
		return errors.New("release audit document exceeds its allowance")
	}
	name, validator := "findings.json", "validate-findings.cjs"
	if kind == "ledger" {
		name, validator = "coverage-ledger.json", "validate-coverage-ledger.cjs"
	}
	if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "node")
	command.Args = append(command.Args, filepath.Join(directory, "bundle", validator), filepath.Join(directory, name))
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + directory, "TMPDIR=" + directory, "TMP=" + directory, "TEMP=" + directory}
	if err := command.Run(); err != nil {
		return fmt.Errorf("validate release audit %s: %w", kind, err)
	}
	return nil
}
