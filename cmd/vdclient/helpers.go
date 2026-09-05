package main

import (
	"encoding/pem"
	"os"
)

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
