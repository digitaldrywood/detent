package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestPrepareTurnPromptScratchFailure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		file bool
	}{
		{name: "missing directory"},
		{name: "regular file", file: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unavailable")
			if test.file {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prompt, cleanup, err := prepareTurnPrompt(strings.Repeat("x", maxTurnInputBytes), path)
			cleanup()
			var pathErr *os.PathError
			if prompt != "" || !errors.As(err, &pathErr) {
				t.Fatalf("failed prompt write returned inline input: bytes = %d, error = %v", len(prompt), err)
			}
			now := time.Now()
			details, ok := ClassifyCapacityError(startupStageError(err, "turn/start", now, now, 0), nil, now)
			if !ok || details.Kind != backendcapacity.StartupFailureKind {
				t.Fatalf("prompt scratch failure lost instance attribution: %+v, classified = %v", details, ok)
			}
		})
	}
}

// Attachment input items (decisions section 17.1). Text attachments already
// reached the prompt as a data block before the backend is called; what the
// app-server protocol needs here is the image input.

func TestTurnInputItems(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\nbody")
	cases := []struct {
		name        string
		prompt      string
		attachments []runner.AgentAttachment
		wantTypes   []string
	}{
		{name: "prompt only", prompt: "do the thing", wantTypes: []string{"text"}},
		{
			name: "one image", prompt: "look",
			attachments: []runner.AgentAttachment{{Name: "shot.png", MIME: "image/png", Content: png}},
			wantTypes:   []string{"text", "localImage"},
		},
		{
			name: "text attachments are not image input", prompt: "read",
			attachments: []runner.AgentAttachment{{Name: "notes.md", MIME: "text/markdown", Content: []byte("hi")}},
			wantTypes:   []string{"text"},
		},
		{
			name: "two images", prompt: "compare",
			attachments: []runner.AgentAttachment{
				{Name: "a.png", MIME: "image/png", Content: png},
				{Name: "b.png", MIME: "image/png", Content: png},
			},
			wantTypes: []string{"text", "localImage", "localImage"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			items, cleanup := turnInputItems(test.prompt, test.attachments, directory)
			t.Cleanup(cleanup)
			if len(items) != len(test.wantTypes) {
				t.Fatalf("turnInputItems() = %+v, want %d items", items, len(test.wantTypes))
			}
			for i, want := range test.wantTypes {
				if items[i]["type"] != want {
					t.Fatalf("item %d type = %v, want %q", i, items[i]["type"], want)
				}
			}
			if items[0]["text"] != test.prompt {
				t.Fatalf("first item text = %v, want %q", items[0]["text"], test.prompt)
			}
			for _, item := range items[1:] {
				path, ok := item["path"].(string)
				if !ok || !strings.HasPrefix(path, directory) {
					t.Fatalf("image item = %+v, want a path under %s", item, directory)
				}
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read image: %v", err)
				}
				if string(content) != string(png) {
					t.Fatalf("image content = %q, want the attachment bytes", content)
				}
			}
		})
	}
}

// An image the runner cannot write to disk still reaches the provider, as an
// inline data URL rather than a path.
func TestTurnInputItemsFallsBackToAnInlineImage(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\nbody")
	unwritable := filepath.Join(t.TempDir(), "missing", "deeper")
	items, cleanup := turnInputItems("look", []runner.AgentAttachment{{Name: "shot.png", MIME: "image/png", Content: png}}, unwritable)
	t.Cleanup(cleanup)
	if len(items) != 2 || items[1]["type"] != "image" {
		t.Fatalf("turnInputItems() = %+v, want a text item and an inline image", items)
	}
	url, ok := items[1]["imageUrl"].(string)
	if !ok || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("inline image = %+v, want a data URL", items[1])
	}
}

// The temporary copies are removed when the turn is over: a user's file must
// not outlive the turn it was attached to.
func TestTurnInputItemsCleansUp(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	items, cleanup := turnInputItems("look", []runner.AgentAttachment{{Name: "shot.png", MIME: "image/png", Content: []byte("\x89PNG\r\n\x1a\nx")}}, directory)
	path, _ := items[1]["path"].(string)
	if path == "" {
		t.Fatalf("items = %+v, want a written image", items)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat after cleanup = %v, want the file to be gone", err)
	}
}

// A file name from a user never escapes the turn directory.
func TestTurnInputItemsIgnoresTheAttachmentPath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	items, cleanup := turnInputItems("look", []runner.AgentAttachment{
		{ID: "att_1", Name: "../../escape.png", MIME: "image/png", Content: []byte("\x89PNG\r\n\x1a\nx")},
	}, directory)
	t.Cleanup(cleanup)
	path, _ := items[1]["path"].(string)
	if filepath.Dir(filepath.Dir(path)) != directory {
		t.Fatalf("image path = %q, want it inside a turn directory under %s", path, directory)
	}
	if strings.Contains(path, "..") {
		t.Fatalf("image path = %q, want no parent traversal", path)
	}
}

func TestTurnInputItemsWritesEveryImageInsideTheTurnDirectory(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\nx")
	tests := []struct {
		name  string
		files []runner.AgentAttachment
		want  []string
	}{
		{name: "two files of one name", files: []runner.AgentAttachment{{Name: "shot.png"}, {Name: "shot.png"}}, want: []string{"01-shot.png", "02-shot.png"}},
		{name: "dot-prefixed name with a traversing id", files: []runner.AgentAttachment{{ID: "../../escape", Name: ".hidden.png"}}, want: []string{"01-hidden.png"}},
		{name: "only dots", files: []runner.AgentAttachment{{ID: "../x", Name: ".."}}, want: []string{"01-attachment"}},
		{name: "windows traversal", files: []runner.AgentAttachment{{Name: `..\..\evil.png`}}, want: []string{"01-evil.png"}},
		{name: "unsafe characters", files: []runner.AgentAttachment{{Name: "a b:c?.png"}}, want: []string{"01-a_b_c_.png"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for i := range test.files {
				test.files[i].MIME = "image/png"
				test.files[i].Content = png
			}
			items, cleanup := turnInputItems("look", test.files, root)
			t.Cleanup(cleanup)
			if len(items) != len(test.want)+1 {
				t.Fatalf("items = %#v", items)
			}
			for i, want := range test.want {
				path, _ := items[i+1]["path"].(string)
				if filepath.Base(path) != want || filepath.Dir(filepath.Dir(path)) != root {
					t.Fatalf("image %d path = %q, want %s inside a turn directory under %s", i, path, want, root)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("image %d was not written: %v", i, err)
				}
			}
		})
	}
}
