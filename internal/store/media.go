package store

import (
	"database/sql"
	"errors"
	"time"
)

// Media is one file in a person's library.
type Media struct {
	ID, Owner, Kind string
	Name, Path      string
	Thumb, Hash     string
	Size            int64
	Duration        float64
	Width, Height   int
	HasAudio        bool
	Created         time.Time
}

func (s *Store) AddMedia(m Media) error {
	_, err := s.db.Exec(`INSERT INTO media (id, owner, kind, name, path, thumb, hash, size, duration, width, height, has_audio, created)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Owner, m.Kind, m.Name, m.Path, m.Thumb, m.Hash, m.Size, m.Duration, m.Width, m.Height, b2i(m.HasAudio), m.Created.Unix())
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

const mediaCols = `id, owner, kind, name, path, thumb, hash, size, duration, width, height, has_audio, created`

func scanMedia(sc interface{ Scan(...any) error }) (Media, error) {
	var m Media
	var audio int
	var created int64
	err := sc.Scan(&m.ID, &m.Owner, &m.Kind, &m.Name, &m.Path, &m.Thumb, &m.Hash, &m.Size, &m.Duration,
		&m.Width, &m.Height, &audio, &created)
	m.HasAudio, m.Created = audio != 0, time.Unix(created, 0)
	return m, err
}

// GetMedia returns a library item only if it belongs to owner.
func (s *Store) GetMedia(owner, id string) (Media, error) {
	m, err := scanMedia(s.db.QueryRow(`SELECT `+mediaCols+` FROM media WHERE id = ? AND owner = ?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

func (s *Store) MediaByHash(owner, kind, hash string) (Media, error) {
	m, err := scanMedia(s.db.QueryRow(`SELECT `+mediaCols+` FROM media WHERE owner = ? AND kind = ? AND hash = ? LIMIT 1`, owner, kind, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

func (s *Store) ListMedia(owner, kind string) ([]Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media WHERE owner = ?`
	args := []any{owner}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q+` ORDER BY created DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMedia(owner, id string) error {
	_, err := s.db.Exec(`DELETE FROM media WHERE id = ? AND owner = ?`, id, owner)
	return err
}

// MediaBytes is the total library size for an owner (for quotas and display).
func (s *Store) MediaBytes(owner string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`SELECT SUM(size) FROM media WHERE owner = ?`, owner).Scan(&n)
	return n.Int64, err
}
