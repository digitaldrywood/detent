package spritenames

import (
	"errors"
	"strconv"
)

var spriteAdjectives = []string{
	"bright", "calm", "clever", "cozy", "curious", "eager", "gentle", "happy",
	"jolly", "kind", "lively", "merry", "nimble", "patient", "playful", "polite",
	"proud", "quiet", "ready", "shiny", "snug", "spry", "steady", "sunny",
	"swift", "tidy", "trusty", "vivid", "warm", "wise", "witty", "zippy",
}

var spriteNouns = []string{
	"badger", "beaver", "bison", "bunny", "canary", "crane", "dolphin", "dove",
	"falcon", "finch", "fox", "gecko", "heron", "koala", "lark", "lemur",
	"lynx", "otter", "owl", "panda", "parrot", "puffin", "quail", "rabbit",
	"robin", "seal", "sparrow", "swan", "turtle", "walrus", "whale", "wren",
}

var ErrTaken = errors.New("sprite name is already taken")
var ErrExhausted = errors.New("could not find an available Sprite name")

type Generator struct {
	intN     func(int) int
	attempts int
}

func New(intN func(int) int) *Generator { return &Generator{intN: intN} }

func (g *Generator) Next(taken func(string) (bool, error)) (string, error) {
	for g.attempts < 10 {
		name := "sprite-" + spriteAdjectives[g.intN(len(spriteAdjectives))] + "-" + spriteNouns[g.intN(len(spriteNouns))]
		if g.attempts >= 5 {
			name += "-" + strconv.FormatInt(int64(g.intN(36*36*36*36*36)), 36)
		}
		g.attempts++
		exists, err := taken(name)
		if err != nil {
			return "", err
		}
		if !exists {
			return name, nil
		}
	}
	return "", ErrExhausted
}

func ValidSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 63 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for _, c := range slug {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
