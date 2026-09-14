package codex

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/runner"
)

// Attachment input for the app-server protocol (decisions section 17.1). A
// turn's input is a list of items; the prompt is the text item and each image
// the user attached is an image item after it. Text attachments are not here:
// the runner has already folded them into the prompt as a delimited data
// block, which is what keeps them data rather than instructions.
//
// An image is handed over as a localImage naming a file the runner wrote for
// this turn, because that is what the protocol prefers for a local file and
// it keeps a large image out of the JSON-RPC frame. When the file cannot be
// written the image still travels, inline, as a data URL.

// turnInputDirPrefix names the per-turn directory the copies live in.
const turnInputDirPrefix = "detent-attachments-"

// turnInputItems renders one turn's input. The returned cleanup removes the
// temporary copies and must be called when the turn is over; it is safe to
// call more than once.
func turnInputItems(prompt string, attachments []runner.AgentAttachment, tempDir string) ([]map[string]any, func()) {
	items := []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
	images := runner.AttachmentImages(attachments)
	if len(images) == 0 {
		return items, func() {}
	}
	directory, err := os.MkdirTemp(tempDir, turnInputDirPrefix)
	cleanup := func() {}
	if err == nil {
		cleanup = func() {
			// The turn is already answered by the time this runs, and the
			// directory sits under the runner's own temp root, so a failed
			// removal is worth saying and nothing more.
			if removeErr := os.RemoveAll(directory); removeErr != nil {
				slog.Warn("codex.attachment_cleanup_failed", "directory", directory, "error", removeErr)
			}
		}
	}
	for _, image := range images {
		if err == nil {
			// The user's file name is a label, never a path: only its base
			// is used, and the turn directory is the whole location.
			path := filepath.Join(directory, attachmentFileName(image))
			if writeErr := os.WriteFile(path, image.Content, 0o600); writeErr == nil {
				items = append(items, map[string]any{"type": "localImage", "path": path})
				continue
			}
		}
		items = append(items, map[string]any{
			"type":     "image",
			"imageUrl": "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Content),
		})
	}
	return items, cleanup
}

// attachmentFileName reduces an attachment to a safe base name inside the
// turn directory. An attachment whose name is unusable is named by its
// identifier instead.
func attachmentFileName(attachment runner.AgentAttachment) string {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(attachment.Name), `\`, "/"))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) || strings.HasPrefix(name, ".") {
		name = strings.TrimSpace(attachment.ID)
	}
	if name == "" {
		name = "attachment"
	}
	return name
}
