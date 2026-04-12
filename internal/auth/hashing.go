package auth

import "github.com/alexedwards/argon2id"

func HashPassword(password string) (string, error) {
	argon2idParams := argon2id.DefaultParams
	return argon2id.CreateHash(password, argon2idParams)
}

func CheckPasswordHash(password, hash string) (bool, error) {
	return argon2id.ComparePasswordAndHash(password, hash)
}
