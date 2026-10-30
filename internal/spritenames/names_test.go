package spritenames

import (
	"errors"
	"regexp"
	"testing"
)

func TestSpriteNames(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^sprite-[a-z]+-[a-z]+(-[a-z0-9]+)?$`)
	lookupErr := errors.New("name lookup failed")
	for _, test := range []struct {
		name   string
		values []int
		taken  map[string]bool
		fail   error
		want   string
		err    error
	}{
		{name: "free pair has no suffix", values: []int{0}, want: "sprite-bright-badger"},
		{name: "different free pair", values: []int{0, 0, 1, 1}, taken: map[string]bool{"sprite-bright-badger": true}, want: "sprite-calm-beaver"},
		{name: "repeated pair gets suffix", values: []int{0}, taken: map[string]bool{"sprite-bright-badger": true}, want: "sprite-bright-badger-0"},
		{name: "taken suffix is retried", values: []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, taken: map[string]bool{"sprite-bright-badger": true, "sprite-bright-badger-0": true}, want: "sprite-bright-badger-1"},
		{name: "suffix space upper boundary", values: []int{-1}, taken: map[string]bool{"sprite-zippy-wren": true}, want: "sprite-zippy-wren-zzzzz"},
		{name: "exhausted names", values: []int{0}, taken: map[string]bool{"sprite-bright-badger": true, "sprite-bright-badger-0": true}, err: ErrExhausted},
		{name: "lookup failure", values: []int{0}, fail: lookupErr, err: lookupErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := 0
			names := Generator{intN: func(n int) int {
				value := test.values[index%len(test.values)]
				index++
				if value < 0 {
					return n - 1
				}
				return value
			}}
			got, err := names.Next(func(name string) (bool, error) { return test.taken[name], test.fail })
			if got != test.want || !errors.Is(err, test.err) {
				t.Fatalf("name=%q err=%v, want %q err=%v", got, err, test.want, test.err)
			}
			if err == nil && (test.taken[got] || !shape.MatchString(got) || !ValidSlug(got) || len(got) > 63) {
				t.Fatalf("invalid or taken name %q", got)
			}
		})
	}
}

func TestSpriteNameWords(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		words []string
	}{
		{"adjectives", spriteAdjectives},
		{"nouns", spriteNouns},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := make(map[string]bool)
			for _, word := range test.words {
				if word == "" || seen[word] {
					t.Fatalf("empty or duplicate word %q", word)
				}
				seen[word] = true
				for _, letter := range word {
					if letter < 'a' || letter > 'z' {
						t.Fatalf("word %q is not lowercase ASCII", word)
					}
				}
			}
		})
	}
	for _, adjective := range spriteAdjectives {
		for _, noun := range spriteNouns {
			if name := "sprite-" + adjective + "-" + noun + "-zzzzz"; !ValidSlug(name) {
				t.Fatalf("word pair exceeds slug limits: %q", name)
			}
		}
	}
}
