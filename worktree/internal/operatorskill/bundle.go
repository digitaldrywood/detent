package operatorskill

import (
	"bytes"
	_ "embed"
)

const (
	Name      = "detent-operator-introspection"
	Version   = 2
	Directory = Name
	SkillFile = "SKILL.md"
)

//go:embed detent-operator-introspection/SKILL.md
var content []byte

func Content() []byte {
	return bytes.Clone(content)
}
