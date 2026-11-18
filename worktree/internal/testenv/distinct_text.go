package testenv

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

func DistinctText(seed string, count int) string {
	words := make([]string, count)
	for index := range words {
		sum := sha256.Sum256([]byte(seed + "\x00" + strconv.Itoa(index)))
		words[index] = "q" + hex.EncodeToString(sum[:6])
	}
	return strings.Join(words, " ")
}
