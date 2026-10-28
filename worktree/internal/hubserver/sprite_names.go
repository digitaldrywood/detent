package hubserver

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

var errSpriteNameTaken = errors.New("sprite name is already taken")
var errSpriteNamesExhausted = errors.New("could not find an available Sprite name")

type spriteNameGenerator struct {
	intN     func(int) int
	attempts int
}

func (g *spriteNameGenerator) next(taken func(string) (bool, error)) (string, error) {
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
	return "", errSpriteNamesExhausted
}
