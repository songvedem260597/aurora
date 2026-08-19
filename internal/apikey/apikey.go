package apikey

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	Prefix     = "sk-gw-"
	DefaultTTL = 24 * time.Hour
)

var (
	ErrInvalid = errors.New("invalid api key")
	ErrExpired = errors.New("api key expired")
)

// Issue creates a stateless API key signed with secret. The caller controls
// the TTL; public handlers use DefaultTTL (24 hours).
func Issue(secret string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", time.Time{}, errors.New("api key signing secret is empty")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	expiresAt := now.UTC().Add(ttl)
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}

	exp := strconv.FormatInt(expiresAt.Unix(), 10)
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	payload := exp + "." + nonceText
	sig := sign(secret, payload)
	return Prefix + payload + "." + sig, expiresAt, nil
}

// Validate checks signature and expiration. It does not keep any server-side
// state, so a key remains valid across process restarts until it expires.
func Validate(secret, token string, now time.Time) error {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(token, Prefix) {
		return ErrInvalid
	}

	raw := strings.TrimPrefix(token, Prefix)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ErrInvalid
	}
	expText, nonceText, gotSig := parts[0], parts[1], parts[2]
	if expText == "" || nonceText == "" || gotSig == "" {
		return ErrInvalid
	}
	if _, err := base64.RawURLEncoding.DecodeString(nonceText); err != nil {
		return ErrInvalid
	}

	payload := expText + "." + nonceText
	wantSig := sign(secret, payload)
	if subtle.ConstantTimeCompare([]byte(gotSig), []byte(wantSig)) != 1 {
		return ErrInvalid
	}

	expUnix, err := strconv.ParseInt(expText, 10, 64)
	if err != nil {
		return ErrInvalid
	}
	expiresAt := time.Unix(expUnix, 0).UTC()
	if !now.UTC().Before(expiresAt) {
		return ErrExpired
	}
	return nil
}

func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
