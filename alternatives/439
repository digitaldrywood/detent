package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"math/big"
	"strings"
)

const (
	TokenPrefix       = "detent_"
	tokenBodyLength   = 43
	tokenCRC32Length  = 6
	tokenSecretLength = tokenBodyLength + tokenCRC32Length
	TokenLength       = len(TokenPrefix) + tokenSecretLength
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	ErrTokenFormat   = errors.New("invalid token format")
	ErrTokenChecksum = errors.New("invalid token checksum")
)

func GenerateToken() (string, error) {
	return GenerateTokenFromReader(rand.Reader)
}

func GenerateTokenFromReader(reader io.Reader) (string, error) {
	random := make([]byte, 32)
	if _, err := io.ReadFull(reader, random); err != nil {
		return "", err
	}
	body := base62EncodeBytes(random, tokenBodyLength)
	checksum := tokenChecksum(body)
	return TokenPrefix + body + checksum, nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func PrefixLast4(token string) string {
	token = strings.TrimSpace(token)
	if len(token) < len(TokenPrefix)+6 {
		return token
	}
	body := strings.TrimPrefix(token, TokenPrefix)
	if len(body) < 6 {
		return token
	}
	return TokenPrefix + body[:2] + "…" + body[len(body)-4:]
}

func ValidateTokenFormat(token string) error {
	token = strings.TrimSpace(token)
	if len(token) != TokenLength || !strings.HasPrefix(token, TokenPrefix) {
		return ErrTokenFormat
	}
	secret := strings.TrimPrefix(token, TokenPrefix)
	for _, r := range secret {
		if !strings.ContainsRune(base62Alphabet, r) {
			return ErrTokenFormat
		}
	}
	body := secret[:tokenBodyLength]
	checksum := secret[tokenBodyLength:]
	if checksum != tokenChecksum(body) {
		return ErrTokenChecksum
	}
	return nil
}

func base62EncodeBytes(raw []byte, length int) string {
	n := new(big.Int).SetBytes(raw)
	return base62EncodeInt(n, length)
}

func base62EncodeUint32(value uint32, length int) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], value)
	n := new(big.Int).SetBytes(raw[:])
	return base62EncodeInt(n, length)
}

func base62EncodeInt(n *big.Int, length int) string {
	if length <= 0 {
		return ""
	}
	base := big.NewInt(62)
	zero := big.NewInt(0)
	current := new(big.Int).Set(n)
	chars := make([]byte, 0, length)
	for current.Cmp(zero) > 0 {
		mod := new(big.Int)
		current.DivMod(current, base, mod)
		chars = append(chars, base62Alphabet[mod.Int64()])
	}
	for len(chars) < length {
		chars = append(chars, base62Alphabet[0])
	}
	if len(chars) > length {
		chars = chars[:length]
	}
	for i, j := 0, len(chars)-1; i < j; i, j = i+1, j-1 {
		chars[i], chars[j] = chars[j], chars[i]
	}
	return string(chars)
}

func tokenChecksum(body string) string {
	return base62EncodeUint32(crc32.ChecksumIEEE([]byte(body)), tokenCRC32Length)
}
