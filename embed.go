package detent

import (
	"embed"
	"io/fs"
)

const operatorSkillPath = "internal/operatorskill/detent-operator-introspection/SKILL.md"

//go:embed static/** internal/operatorskill/detent-operator-introspection/SKILL.md
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
