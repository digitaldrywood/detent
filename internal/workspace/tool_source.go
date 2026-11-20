package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

const toolSourceReadLimit = 8 << 20

func ToolSourceFingerprint(ctx context.Context, path string) string {
	index, truncated, err := gitDiffArgsWithinLimit(ctx, path, nil, toolSourceReadLimit, gitDiagnosticArgs("ls-files", "--stage", "-z")...)
	if err != nil || truncated || index == "" {
		return ""
	}
	working, err := gitDiffFingerprint(ctx, path, nil, "--no-textconv", "--ignore-submodules=none")
	if err != nil {
		return ""
	}
	untracked, truncated, err := gitDiffArgsWithinLimit(ctx, path, nil, toolSourceReadLimit, gitDiagnosticArgs("ls-files", "--others", "--exclude-standard", "-z")...)
	if err != nil || truncated {
		return ""
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return ""
	}
	defer root.Close()
	digest := sha256.New()
	remaining := int64(toolSourceReadLimit)
	for name := range strings.SplitSeq(strings.TrimSuffix(untracked, "\x00"), "\x00") {
		if name == "" {
			continue
		}
		if ctx.Err() != nil {
			return ""
		}
		info, err := root.Lstat(name)
		if err != nil {
			return ""
		}
		fmt.Fprintf(digest, "%d:%s:%d:", len(name), name, info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := root.Readlink(name)
			if err != nil {
				return ""
			}
			fmt.Fprintf(digest, "%d:%s", len(target), target)
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > remaining {
			return ""
		}
		file, err := root.Open(name)
		if err != nil {
			return ""
		}
		fmt.Fprintf(digest, "%d:", info.Size())
		copied, readErr := io.Copy(digest, io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || copied != info.Size() {
			return ""
		}
		remaining -= copied
	}
	return workspaceRecoveryFingerprint(index, working, hex.EncodeToString(digest.Sum(nil)))
}
