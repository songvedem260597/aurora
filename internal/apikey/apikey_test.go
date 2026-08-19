package apikey

import (
	"errors"
	"testing"
	"time"
)

func TestIssueAndValidateForOneDay(t *testing.T) {
	now := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	key, expiresAt, err := Issue("master-secret", now, DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got, want := expiresAt.Sub(now), 24*time.Hour; got != want {
		t.Fatalf("ttl = %v, want %v", got, want)
	}
	if err := Validate("master-secret", key, now.Add(23*time.Hour+59*time.Minute)); err != nil {
		t.Fatalf("key should still be valid: %v", err)
	}
	if err := Validate("master-secret", key, expiresAt); !errors.Is(err, ErrExpired) {
		t.Fatalf("key must expire exactly at expires_at: %v", err)
	}
}

func TestValidateRejectsTamperedKey(t *testing.T) {
	now := time.Now().UTC()
	key, _, err := Issue("master-secret", now, DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	key = key[:len(key)-1] + "x"
	if err := Validate("master-secret", key, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered key error = %v, want ErrInvalid", err)
	}
}

func TestValidateRejectsWrongSecret(t *testing.T) {
	now := time.Now().UTC()
	key, _, err := Issue("master-secret", now, DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := Validate("other-secret", key, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong-secret error = %v, want ErrInvalid", err)
	}
}
