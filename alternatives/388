package templates

import "embed"

//go:embed WORKFLOW.*.md detent.*.yaml onboarding-legacy-paragraphs.md
var FS embed.FS

// BlockedHandoff is the canonical worker status contract. The runner replaces
// completion_fields with the current attempt's completion identity.
//
//go:embed blocked-handoff.md
var BlockedHandoff string
