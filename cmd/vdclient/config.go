package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

func loadToken(path string) ([32]byte, error) {
	var token [32]byte
	data, err := os.ReadFile(path)
	if err != nil {
		return token, err
	}
	if len(data) == len(token) {
		copy(token[:], data)
		return token, nil
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != len(token) {
		return token, errors.New("token file must contain exactly 32 raw bytes or 64 hexadecimal characters")
	}
	copy(token[:], decoded)
	return token, nil
}

func parseFingerprint(value string) ([sha256.Size]byte, error) {
	var fingerprint [sha256.Size]byte
	normalized := strings.ReplaceAll(strings.TrimSpace(value), ":", "")
	decoded, err := hex.DecodeString(normalized)
	if err != nil || len(decoded) != len(fingerprint) {
		return fingerprint, errors.New("certificate fingerprint must contain exactly 64 hexadecimal characters")
	}
	copy(fingerprint[:], decoded)
	return fingerprint, nil
}
