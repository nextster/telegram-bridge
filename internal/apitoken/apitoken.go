// Package apitoken creates personal API tokens. Only SHA-256 hashes are stored;
// a token is shown to its owner once.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/nextster/telegram-bridge/internal/db"
)

var prefixes = map[string]string{
	db.APITokenScopeMCP:    "tbm_",
	db.APITokenScopeNotify: "tbn_",
}

// New returns a token for scope and its storage hash.
func New(scope string) (string, string, error) {
	prefix, ok := prefixes[scope]
	if !ok {
		return "", "", fmt.Errorf("unknown token scope %q", scope)
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", "", err
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(buffer)
	return token, Hash(token), nil
}

func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
