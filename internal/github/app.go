package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v83/github"
)

// App exchanges a GitHub App identity for short-lived installation tokens.
type App struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	httpClient     *http.Client
	baseURL        string
	now            func() time.Time
}

// NewApp returns a GitHub App installation-token provider.
func NewApp(appID, installationID int64, privateKeyPEM []byte) (*App, error) {
	return newApp(appID, installationID, privateKeyPEM, &http.Client{Timeout: 30 * time.Second}, "", time.Now)
}

func newApp(appID, installationID int64, privateKeyPEM []byte, httpClient *http.Client, baseURL string, now func() time.Time) (*App, error) {
	if appID <= 0 || installationID <= 0 {
		return nil, errors.New("GitHub App ID and installation ID must be positive")
	}

	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("parse GitHub App private key: PEM block is required")
	}

	privateKey, err := parseRSAPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}

	return &App{appID: appID, installationID: installationID, privateKey: privateKey, httpClient: httpClient, baseURL: baseURL, now: now}, nil
}

// InstallationToken returns a fresh token for the configured installation.
func (a *App) InstallationToken(ctx context.Context) (string, error) {
	identity, err := a.signedIdentity()
	if err != nil {
		return "", err
	}

	client := gh.NewClient(a.httpClient).WithAuthToken(identity)
	if a.baseURL != "" {
		baseURL, err := client.BaseURL.Parse(a.baseURL)
		if err != nil {
			return "", fmt.Errorf("configure GitHub API URL: %w", err)
		}

		client.BaseURL = baseURL
	}

	result, _, err := client.Apps.CreateInstallationToken(ctx, a.installationID, nil)
	if err != nil {
		return "", fmt.Errorf("create GitHub App installation token: %w", err)
	}

	token := strings.TrimSpace(result.GetToken())
	if token == "" {
		return "", errors.New("GitHub App returned an empty installation token")
	}

	return token, nil
}

func (a *App) signedIdentity() (string, error) {
	now := a.now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode GitHub App JWT header: %w", err)
	}

	payload, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(9 * time.Minute).Unix(), Issuer: a.appID})
	if err != nil {
		return "", fmt.Errorf("encode GitHub App JWT payload: %w", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseRSAPrivateKey(contents []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(contents); err == nil {
		return key, nil
	}

	key, err := x509.ParsePKCS8PrivateKey(contents)
	if err != nil {
		return nil, err
	}

	privateKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}

	return privateKey, nil
}
