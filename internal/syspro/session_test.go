package syspro

import (
	"context"
	"testing"
)

func TestOpenSession_Success(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer session.Close(context.Background()) //nolint:errcheck

	if fake.logonCalls != 1 {
		t.Errorf("expected 1 logon call, got %d", fake.logonCalls)
	}
}

func TestOpenSession_LogonFailure(t *testing.T) {
	fake := newFakeEnet(t)
	fake.logonErr = true
	c := fake.client(t)

	_, err := c.OpenSession(context.Background())
	if err == nil {
		t.Fatal("expected error on logon failure")
	}
}

func TestSession_SubmitOrder_Success(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close(context.Background()) //nolint:errcheck

	result, err := session.SubmitOrder(context.Background(), testOrder(), testLines())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got error: %s", result.ErrorMessage)
	}
	if result.SysproOrderNumber != "SO12345" {
		t.Errorf("expected SO12345, got %q", result.SysproOrderNumber)
	}
}

func TestSession_MultipleSubmits_ReuseSession(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer session.Close(context.Background()) //nolint:errcheck

	for i := 0; i < 3; i++ {
		_, err := session.SubmitOrder(context.Background(), testOrder(), testLines())
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	if fake.logonCalls != 1 {
		t.Errorf("expected 1 logon call (reused session), got %d", fake.logonCalls)
	}
	if fake.transactCalls != 3 {
		t.Errorf("expected 3 transaction calls, got %d", fake.transactCalls)
	}
}

func TestSession_Close(t *testing.T) {
	fake := newFakeEnet(t)
	c := fake.client(t)

	session, err := c.OpenSession(context.Background())
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if fake.logoffCalls != 1 {
		t.Errorf("expected 1 logoff call, got %d", fake.logoffCalls)
	}
}
