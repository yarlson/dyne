package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppInstallationTokenUsesSignedAppIdentity(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		assert.Equal(t, "/app/installations/456/access_tokens", request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"token":"installation-token","expires_at":"2026-08-29T12:00:00Z"}`))
	}))
	defer server.Close()

	app, err := newApp(123, 456, keyPEM, server.Client(), server.URL+"/", func() time.Time {
		return time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	})
	require.NoError(t, err)
	token, err := app.InstallationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "installation-token", token)
	assert.Regexp(t, `^Bearer [^.]+\.[^.]+\.[^.]+$`, authorization)
}

func TestNewAppRejectsInvalidPrivateKey(t *testing.T) {
	_, err := NewApp(123, 456, []byte("not a key"))
	require.ErrorContains(t, err, "parse GitHub App private key")
}
