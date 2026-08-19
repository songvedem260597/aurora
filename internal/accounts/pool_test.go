package accounts

import (
	"testing"
	"time"
)

func TestPoolAcquireByType(t *testing.T) {
	pool := NewPool(nil)

	a1 := NewAccount("noauth-1", TypeNoAuth, "uuid-1")
	a2 := NewAccount("free-1", TypeFree, "token-free-1")
	a3 := NewAccount("puid-1", TypePUID, "token-puid-1")

	a1.Status = StatusActive
	a2.Status = StatusActive
	a3.Status = StatusActive

	pool.AddAccount(a1)
	pool.AddAccount(a2)
	pool.AddAccount(a3)

	acct, err := pool.Acquire(TypePUID)
	if err != nil {
		t.Fatalf("Acquire PUID: %v", err)
	}
	if acct.Type != TypePUID {
		t.Errorf("got type %s, want puid", acct.Type)
	}

	acct, err = pool.Acquire(TypeNoAuth)
	if err != nil {
		t.Fatalf("Acquire NoAuth: %v", err)
	}
	if acct.Type != TypeNoAuth {
		t.Errorf("got type %s, want noauth", acct.Type)
	}
}

func TestPoolTemporarilySkipsRateLimitedAccount(t *testing.T) {
	pool := NewPool(nil)
	limited := NewAccount("limited", TypeFree, "token-1")
	fallback := NewAccount("fallback", TypeFree, "token-2")
	limited.Status = StatusActive
	fallback.Status = StatusActive
	pool.AddAccount(limited)
	pool.AddAccount(fallback)

	first, err := pool.Acquire(TypeFree)
	if err != nil || first != limited {
		t.Fatalf("first Acquire = %v, %v; want limited account", first, err)
	}
	if !pool.ReportAttachmentLimited(limited, time.Hour) {
		t.Fatal("managed account was not marked attachment limited")
	}
	next, err := pool.AcquireForAttachments(TypeFree)
	if err != nil || next != fallback {
		t.Fatalf("fallback Acquire = %v, %v; want fallback account", next, err)
	}

	textAccount, err := pool.Acquire(TypeFree)
	if err != nil || textAccount != limited {
		t.Fatalf("text Acquire = %v, %v; attachment quota must not disable text", textAccount, err)
	}

	fallback.Status = StatusDisabled
	limited.AttachmentLimitedUntil = time.Now().Add(-time.Second)
	recovered, err := pool.AcquireForAttachments(TypeFree)
	if err != nil || recovered != limited {
		t.Fatalf("recovered Acquire = %v, %v; want cooled-down account", recovered, err)
	}
	if limited.Status != StatusActive || !limited.AttachmentLimitedUntil.IsZero() {
		t.Fatalf("limited account did not recover: status=%s until=%v", limited.Status, limited.AttachmentLimitedUntil)
	}
}

func TestPoolDoesNotRateLimitUnmanagedAccount(t *testing.T) {
	pool := NewPool(nil)
	external := NewAccount("external", TypeFree, "token")
	external.Status = StatusActive
	if pool.ReportAttachmentLimited(external, time.Hour) {
		t.Fatal("unmanaged caller credential must not be added to pool rotation")
	}
	if external.Status != StatusActive {
		t.Fatalf("unmanaged account status changed to %s", external.Status)
	}
}

func TestPoolAcquireRoundRobin(t *testing.T) {
	pool := NewPool(nil)
	a1 := NewAccount("a1", TypeNoAuth, "1")
	a2 := NewAccount("a2", TypeNoAuth, "2")
	a1.Status = StatusActive
	a2.Status = StatusActive
	pool.AddAccount(a1)
	pool.AddAccount(a2)

	first, _ := pool.Acquire(TypeNoAuth)
	first.TotalCalls++
	_, _ = pool.Acquire(TypeNoAuth)
}

func TestPoolAcquireNoAvailable(t *testing.T) {
	pool := NewPool(nil)
	_, err := pool.Acquire(TypePUID)
	if err == nil {
		t.Fatal("expected error when no accounts available")
	}
}

func TestPoolReleaseUpdatesStats(t *testing.T) {
	pool := NewPool(nil)
	acct := NewAccount("test", TypeFree, "token")
	acct.Status = StatusActive
	pool.AddAccount(acct)

	// Acquire 会自增 TotalCalls
	got, err := pool.Acquire(TypeFree)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1", got.TotalCalls)
	}
	if got.FailedCalls != 0 {
		t.Errorf("FailedCalls = %d, want 0", got.FailedCalls)
	}
}
