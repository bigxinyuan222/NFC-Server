package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	passwordScheme      = "pbkdf2_sha256"
	passwordIterations  = 600000
	passwordSaltBytes   = 16
	passwordKeyBytes    = 32
	passwordMaxIterates = 1000000
)

// hashPassword returns a self-contained password hash for database storage.
func hashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	derived := pbkdf2SHA256([]byte(password), salt, passwordIterations, passwordKeyBytes)
	return fmt.Sprintf("%s$%d$%s$%s",
		passwordScheme,
		passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100000 || iterations > passwordMaxIterates {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 8 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) != passwordKeyBytes {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, iterations, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blockCount := (keyLen + hashLen - 1) / hashLen
	derived := make([]byte, 0, blockCount*hashLen)

	for block := 1; block <= blockCount; block++ {
		mac := hmac.New(sha256.New, password)
		var index [4]byte
		binary.BigEndian.PutUint32(index[:], uint32(block))
		mac.Write(salt)
		mac.Write(index[:])
		u := mac.Sum(nil)
		result := append([]byte(nil), u...)

		for i := 1; i < iterations; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range result {
				result[j] ^= u[j]
			}
		}
		derived = append(derived, result...)
	}
	return derived[:keyLen]
}
