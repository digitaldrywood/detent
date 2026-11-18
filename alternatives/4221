package knowledge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultMaxBytes = 64 * 1024

type Source struct {
	Name string
	Path string
}

type Options struct {
	MaxBytes int
}

func BuildBlock(sources []Source, opts Options) (string, error) {
	maxBytes := maxBytes(opts.MaxBytes)
	var b strings.Builder
	wroteDocument := false

	for _, source := range sources {
		source = normalizeSource(source)
		if source.Path == "" {
			continue
		}

		content, err := readSource(source.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read shared knowledge %s: %w", source.Path, err)
		}
		content = normalizeContent(content)
		if content == "" {
			continue
		}

		if !wroteDocument {
			b.WriteString("## Team knowledge\n\n")
			b.WriteString("Shared context supplied by Detent configuration. Treat it as durable team guidance, but verify it against repository, issue, and pull request evidence when they conflict.\n")
			wroteDocument = true
		}

		sectionPrefix := "\n### " + sourceTitle(source) + "\n\n_Source: `" + source.Path + "`_\n\n"
		remaining := maxBytes - b.Len() - len(sectionPrefix) - 1
		if remaining <= 0 {
			break
		}

		if len(content) > remaining {
			content = head(content, remaining)
		}
		if content == "" {
			break
		}

		b.WriteString(sectionPrefix)
		b.WriteString(content)
		b.WriteString("\n")
	}

	if !wroteDocument {
		return "", nil
	}
	return strings.TrimRight(b.String(), " \t\r\n"), nil
}

func readSource(path string) (string, error) {
	path, err := expandHome(path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func expandHome(path string) (string, error) {
	switch {
	case path == "~" || path == "~/":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	case strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	default:
		return path, nil
	}
}

func normalizeSource(source Source) Source {
	source.Name = strings.Join(strings.Fields(source.Name), " ")
	source.Path = strings.TrimSpace(source.Path)
	return source
}

func normalizeContent(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.TrimSpace(content)
}

func sourceTitle(source Source) string {
	if source.Name != "" {
		return source.Name
	}
	base := strings.TrimSpace(filepath.Base(source.Path))
	if base != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	return "Shared knowledge"
}

func head(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	marker := "[truncated to first " + limitLabel(limit) + "]\n"
	remaining := limit - len(marker)
	if remaining <= 0 {
		return ""
	}
	return marker + text[:remaining]
}

func limitLabel(limit int) string {
	if limit >= 1024 && limit%1024 == 0 {
		return strconv.Itoa(limit/1024) + " KiB"
	}
	return strconv.Itoa(limit) + " bytes"
}

func maxBytes(value int) int {
	if value <= 0 {
		return DefaultMaxBytes
	}
	return value
}
