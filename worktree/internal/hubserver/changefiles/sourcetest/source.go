package sourcetest

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func Pack(t *testing.T, kind byte, size uint64, content []byte, count uint32) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.WriteString("PACK")
	pack.Write(binary.BigEndian.AppendUint32(nil, 2))
	pack.Write(binary.BigEndian.AppendUint32(nil, count))
	if kind != 0 {
		for range count {
			remaining := size >> 4
			value := kind<<4 | byte(size&15)
			for remaining != 0 {
				pack.WriteByte(value | 128)
				value, remaining = byte(remaining&127), remaining>>7
			}
			pack.WriteByte(value)
			if kind == 6 {
				pack.WriteByte(1)
			}
			writer := zlib.NewWriter(&pack)
			if _, err := writer.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	pack.Write(make([]byte, 20))
	return pack.Bytes()
}

func Source(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	bundle, _ := Checkout(t, input, content...)
	return bundle
}

func Checkout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "-b", "main", "--object-format=sha1", "--template=")
	path := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(path, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	data := []byte(input.HeadSHA + "\n")
	if len(content) > 0 && content[0] != nil {
		data = content[0]
		if err := os.WriteFile(path, append(bytes.Clone(data), []byte("draft source\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
		git("commit", "-m", "source draft")
	}
	if len(content) == 0 || content[0] != nil {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
	}
	git("commit", "--allow-empty", "-m", "reviewed source")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	bundlePath := filepath.Join(directory, "source.bundle")
	git("bundle", "create", bundlePath, "HEAD", "^"+base)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	diff := git("diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	input.BaseSHA, input.HeadSHA, input.MergeBaseSHA = base, head, base
	input.Source = &tracker.ChangeSource{
		Format: "git-bundle", BaseSHA: base, HeadSHA: head,
		BundleSHA256: tracker.ChangeSourceDigest(bundle),
		DiffSHA256:   tracker.ChangeSourceDigest([]byte(diff)),
		Bytes:        int64(len(bundle)),
	}
	return bundle, directory
}
