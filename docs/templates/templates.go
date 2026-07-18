package templates

import "embed"

//go:embed WORKFLOW.*.md detent.*.yaml
var FS embed.FS
