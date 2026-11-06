package gotestreceipt

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	treeInput        = "tree"
	maxLoggedInputs  = 64 << 10
	treeListingChunk = 256
)

func compactInputs(inputs []string, env Environment) []string {
	if len(inputs) > maxLoggedInputs || ValidateInputs(inputs) == nil {
		return inputs
	}
	sizes := make([]int, len(inputs))
	total := 1
	for i, input := range inputs {
		sizes[i] = encodedLength(input)
		total += sizes[i]
	}
	groups := make(map[string][]int)
	for i, input := range inputs {
		fields := strings.Split(input, "\t")
		if len(fields) != 3 || fields[0] != "open" {
			continue
		}
		location, err := env.path(fields[1], fields[2])
		if err != nil {
			continue
		}
		if info, err := os.Lstat(location); err != nil || !info.Mode().IsRegular() {
			continue
		}
		key := fields[1] + "\t" + path.Dir(fields[2])
		groups[key] = append(groups[key], i)
	}
	keys := make([]string, 0, len(groups))
	for key, members := range groups {
		if len(members) > 1 {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(groups[b]), len(groups[a])), strings.Compare(a, b))
	})
	covered := make([]bool, len(inputs))
	count := len(inputs)
	var trees []string
	for _, key := range keys {
		if count <= MaxInputs && total <= maxInputBytes {
			break
		}
		for _, member := range groups[key] {
			covered[member] = true
			total -= sizes[member]
		}
		tree := treeInput + "\t" + key
		trees = append(trees, tree)
		total += encodedLength(tree)
		count += 1 - len(groups[key])
	}
	compacted := make([]string, 0, count)
	for i, input := range inputs {
		if !covered[i] {
			compacted = append(compacted, input)
		}
	}
	compacted = append(compacted, trees...)
	slices.Sort(compacted)
	return compacted
}

func encodedLength(input string) int {
	encoded, err := json.Marshal(input)
	if err != nil {
		return len(input) + 3
	}
	return len(encoded) + 1
}

func listTree(dir string, limit int) ([]os.DirEntry, error) {
	handle, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open test input directory: %w", err)
	}
	var entries []os.DirEntry
	for {
		chunk, readErr := handle.ReadDir(treeListingChunk)
		entries = append(entries, chunk...)
		if len(entries) > limit {
			return nil, errors.Join(errInputEntries, handle.Close())
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, errors.Join(fmt.Errorf("list test input: %w", readErr), handle.Close())
		}
	}
	if err := handle.Close(); err != nil {
		return nil, fmt.Errorf("list test input: %w", err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

func treeNames(entries []os.DirEntry) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name()+"\x00"+entry.Type().String())
	}
	return strings.Join(names, "\n")
}

func (v *Verifier) observeTree(dir string, host bool) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("stat test input directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("test input tree is not a directory")
	}
	entries, err := listTree(dir, min(maxDirectoryEntries, v.entries))
	if err != nil {
		return "", err
	}
	v.entries -= len(entries)
	hash := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(hash, "%s\x00%s", entry.Name(), entry.Type())
		switch {
		case entry.IsDir():
		case entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0:
			value, err := v.observePath("open", filepath.Join(dir, entry.Name()), host)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(hash, "\x00%s", value)
		default:
			return "", errors.New("test input tree contains a special file")
		}
		fmt.Fprintln(hash)
	}
	after, err := listTree(dir, maxDirectoryEntries)
	if err != nil {
		return "", err
	}
	if treeNames(after) != treeNames(entries) {
		return "", errors.New("test input tree changed while it was hashed")
	}
	v.hashedContent = true
	return "tree " + hex.EncodeToString(hash.Sum(nil)), nil
}

func treeModifiedSince(dir, name string, start time.Time) error {
	entries, err := listTree(dir, maxDirectoryEntries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			return errors.New("test input tree contains a special file")
		}
		child := filepath.Join(dir, entry.Name())
		for _, stat := range []func(string) (os.FileInfo, error){os.Lstat, os.Stat} {
			info, err := stat(child)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("stat test input: %w", err)
			}
			if info.ModTime().After(start) {
				return fmt.Errorf("test input %s/%s changed while the test ran", name, entry.Name())
			}
		}
	}
	return nil
}
