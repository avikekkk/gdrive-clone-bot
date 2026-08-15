package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestAuthorizeAndRevoke(t *testing.T) {
	s, _ := openTestStore(t)

	if ok, err := s.IsAuthorized(42); err != nil || ok {
		t.Fatalf("IsAuthorized(42) = (%v, %v), want (false, nil)", ok, err)
	}

	added, err := s.Authorize(42, 1)
	if err != nil || !added {
		t.Fatalf("Authorize(42) = (%v, %v), want (true, nil)", added, err)
	}

	// Re-authorizing an existing ID reports no change.
	if added, err := s.Authorize(42, 1); err != nil || added {
		t.Errorf("second Authorize(42) = (%v, %v), want (false, nil)", added, err)
	}

	if ok, err := s.IsAuthorized(42); err != nil || !ok {
		t.Errorf("IsAuthorized(42) = (%v, %v), want (true, nil)", ok, err)
	}

	removed, err := s.Revoke(42)
	if err != nil || !removed {
		t.Fatalf("Revoke(42) = (%v, %v), want (true, nil)", removed, err)
	}
	if removed, err := s.Revoke(42); err != nil || removed {
		t.Errorf("second Revoke(42) = (%v, %v), want (false, nil)", removed, err)
	}
	if ok, err := s.IsAuthorized(42); err != nil || ok {
		t.Errorf("IsAuthorized(42) after revoke = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestIsAuthorizedAcceptsAnyID(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.Authorize(-1001234567890, 1); err != nil {
		t.Fatal(err)
	}

	// A command is allowed when either the chat or the user is authorized.
	ok, err := s.IsAuthorized(-1001234567890, 777)
	if err != nil || !ok {
		t.Errorf("chat authorization not honored: (%v, %v)", ok, err)
	}
	ok, err = s.IsAuthorized(-100999, 777)
	if err != nil || ok {
		t.Errorf("unrelated IDs should not be authorized: (%v, %v)", ok, err)
	}

	// Zero IDs come from peers we could not resolve and must never match.
	if ok, err := s.IsAuthorized(0); err != nil || ok {
		t.Errorf("IsAuthorized(0) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestAuthorizationsPersistAcrossRestart(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.Authorize(99, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if ok, err := reopened.IsAuthorized(99); err != nil || !ok {
		t.Errorf("authorization did not survive restart: (%v, %v)", ok, err)
	}
}

func TestList(t *testing.T) {
	s, _ := openTestStore(t)
	for _, id := range []int64{1, 2, 3} {
		if _, err := s.Authorize(id, 7); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("List() returned %d entries, want 3", len(entries))
	}
	for _, entry := range entries {
		if entry.AddedBy != 7 {
			t.Errorf("entry %d AddedBy = %d, want 7", entry.ID, entry.AddedBy)
		}
		if entry.AddedAt.IsZero() {
			t.Errorf("entry %d has no timestamp", entry.ID)
		}
	}
}
