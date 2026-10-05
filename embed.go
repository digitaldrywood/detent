package detent

import (
	"embed"
	"io/fs"
)

const operatorSkillPath = "internal/operatorskill/detent-operator-introspection/SKILL.md"

//go:embed static/** static/app/conversation/app.js internal/operatorskill/detent-operator-introspection/SKILL.md .detent/skills/split-issue.md
var embeddedFiles embed.FS

func StaticFS() fs.FS {
	staticFS, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		panic(err)
	}
	return staticFS
}

func OperatorSkillContent() []byte {
	content, err := fs.ReadFile(embeddedFiles, operatorSkillPath)
	if err != nil {
		panic(err)
	}
	return content
}

func SplitIssueSkillContent() []byte {
	content, err := fs.ReadFile(embeddedFiles, ".detent/skills/split-issue.md")
	if err != nil {
		panic(err)
	}
	return content
}
