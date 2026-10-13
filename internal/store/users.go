package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User is an account on this installation.
type User struct {
	ID       int64
	Email    string
	Name     string
	PassHash string
	Created  time.Time
}

var ErrEmailTaken = errors.New("that email is already registered")

func (s *Store) CreateUser(email, name, passHash string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO users (email, name, pass_hash, created) VALUES (?,?,?,?)`, email, name, passHash, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, Name: name, PassHash: passHash, Created: now}, nil
}

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var created int64
	err := sc.Scan(&u.ID, &u.Email, &u.Name, &u.PassHash, &created)
	u.Created = time.Unix(created, 0)
	return u, err
}

func (s *Store) UserByEmail(email string) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT id, email, name, pass_hash, created FROM users WHERE email = ?`,
		strings.ToLower(strings.TrimSpace(email))))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) UserByID(id int64) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT id, email, name, pass_hash, created FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) SetPassword(userID int64, passHash string) error {
	_, err := s.db.Exec(`UPDATE users SET pass_hash = ? WHERE id = ?`, passHash, userID)
	return err
}

// Sessions are stored by the SHA-256 of their token, so a copy of the database
// cannot be used to sign in.
func (s *Store) CreateSession(tokenHash string, userID int64, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created, expires) VALUES (?,?,?,?)`,
		tokenHash, userID, time.Now().Unix(), expires.Unix())
	return err
}

// SessionUser returns the user for a live session and slides its expiry.
func (s *Store) SessionUser(tokenHash string, extendTo time.Time) (User, error) {
	var uid int64
	var expires int64
	err := s.db.QueryRow(`SELECT user_id, expires FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&uid, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if time.Now().Unix() > expires {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return User{}, ErrNotFound
	}
	if extendTo.Unix() > expires+3600 { // slide at most once an hour
		_, _ = s.db.Exec(`UPDATE sessions SET expires = ? WHERE token_hash = ?`, extendTo.Unix(), tokenHash)
	}
	return s.UserByID(uid)
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions signs a user out everywhere (used on password change).
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}
