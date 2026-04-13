package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func generateTestUserID() uuid.UUID {
	return uuid.New()
}

func TestMakeAndValidateJWT(t *testing.T) {
	userID := generateTestUserID()
	tokenSecret := "test-secret"
	expiresIn := 1 * time.Hour

	token, err := MakeJWT(userID, tokenSecret, expiresIn)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	validatedUserID, err := ValidateJWT(token, tokenSecret)
	require.NoError(t, err)
	require.Equal(t, userID, validatedUserID)

	validatedUserID, err = ValidateJWT(token, "wrong-secret")
	require.Error(t, err)
}

func TestValidateExpiredJWT(t *testing.T) {
	userID := generateTestUserID()
	tokenSecret := "test-secret"
	expiresIn := -1 * time.Hour // Token already expired

	token, err := MakeJWT(userID, tokenSecret, expiresIn)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	_, err = ValidateJWT(token, tokenSecret)
	require.Error(t, err)
}
