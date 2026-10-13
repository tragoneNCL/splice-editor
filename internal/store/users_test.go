package store

import (
	"testing"
	"time"
)

func TestUsersAndSessions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	u, err := s.CreateUser("  Ada@Example.com ", "Ada", "hash1")
	if err != nil || u.Email != "ada@example.com" {
		t.Fatalf("create: %+v %v", u, err)
	}
	if _, err := s.CreateUser("ADA@example.com", "", "h"); err != ErrEmailTaken {
		t.Errorf("duplicate email (case-insensitive) err = %v", err)
	}
	got, err := s.UserByEmail("ada@EXAMPLE.com")
	if err != nil || got.ID != u.ID {
		t.Errorf("lookup: %+v %v", got, err)
	}
	if _, err := s.UserByEmail("nobody@example.com"); err != ErrNotFound {
		t.Errorf("missing user err = %v", err)
	}

	if err := s.CreateSession("tok", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if su, err := s.SessionUser("tok", time.Now().Add(time.Hour)); err != nil || su.ID != u.ID {
		t.Errorf("session user: %+v %v", su, err)
	}
	if _, err := s.SessionUser("unknown", time.Now()); err != ErrNotFound {
		t.Errorf("unknown token err = %v", err)
	}
	_ = s.CreateSession("old", u.ID, time.Now().Add(-time.Minute))
	if _, err := s.SessionUser("old", time.Now()); err != ErrNotFound {
		t.Errorf("expired session err = %v", err)
	}
	_ = s.DeleteUserSessions(u.ID)
	if _, err := s.SessionUser("tok", time.Now()); err != ErrNotFound {
		t.Errorf("sessions should be gone after sign-out-everywhere, err = %v", err)
	}
	if n, _ := s.UserCount(); n != 1 {
		t.Errorf("user count = %d", n)
	}
}
