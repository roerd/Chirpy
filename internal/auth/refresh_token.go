package auth

import (
	"crypto/rand"
	"encoding/hex"
)

func MakeRefreshToken() string {
	buf := make([]byte, 32)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}
