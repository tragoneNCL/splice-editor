// Package store persists jobs, edit history and per-visitor preferences in
// SQLite, so edits survive restarts and visitors get their settings back.
// API keys are never stored here.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, builds in a slim container
)

type Store struct{ db *sql.DB }

// Open creates or migrates the database at path (":memory:" for tests).
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; serialising avoids "database is locked"
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY, visitor TEXT NOT NULL DEFAULT '',
			created INTEGER NOT NULL, updated INTEGER NOT NULL,
			status TEXT NOT NULL, stage TEXT NOT NULL DEFAULT '', progress REAL NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '', planner TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 0, parent INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '', preset TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '',
			edl TEXT NOT NULL DEFAULT '', recipe TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '',
			dir TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS jobs_visitor ON jobs(visitor, created DESC)`,
		`CREATE TABLE IF NOT EXISTS revisions (
			job_id TEXT NOT NULL, n INTEGER NOT NULL, parent INTEGER NOT NULL DEFAULT 0,
			edl TEXT NOT NULL, instruction TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL,
			PRIMARY KEY (job_id, n))`,
		`CREATE TABLE IF NOT EXISTS visitors (
			id TEXT PRIMARY KEY, prefs TEXT NOT NULL DEFAULT '{}', created INTEGER NOT NULL, seen INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Job is the persisted form of a job. JSON-typed fields are kept as raw JSON
// so this package does not depend on the editor's types.
type Job struct {
	ID, Visitor        string
	Created, Updated   time.Time
	Status, Stage      string
	Progress           float64
	Error, Planner     string
	Revision, Parent   int
	Title, Preset      string
	Prompt             string
	EDL, Recipe, Notes json.RawMessage
	Dir                string
}

func (s *Store) UpsertJob(j Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs
		(id, visitor, created, updated, status, stage, progress, error, planner, revision, parent, title, preset, prompt, edl, recipe, notes, dir)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			updated=excluded.updated, status=excluded.status, stage=excluded.stage, progress=excluded.progress,
			error=excluded.error, planner=excluded.planner, revision=excluded.revision, parent=excluded.parent,
			edl=excluded.edl, recipe=excluded.recipe, notes=excluded.notes, dir=excluded.dir`,
		j.ID, j.Visitor, j.Created.Unix(), time.Now().Unix(), j.Status, j.Stage, j.Progress, j.Error, j.Planner,
		j.Revision, j.Parent, j.Title, j.Preset, j.Prompt, string(j.EDL), string(j.Recipe), string(j.Notes), j.Dir)
	return err
}

const jobCols = `id, visitor, created, updated, status, stage, progress, error, planner, revision, parent,
	title, preset, prompt, edl, recipe, notes, dir`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var created, updated int64
	var edl, recipe, notes string
	err := sc.Scan(&j.ID, &j.Visitor, &created, &updated, &j.Status, &j.Stage, &j.Progress, &j.Error, &j.Planner,
		&j.Revision, &j.Parent, &j.Title, &j.Preset, &j.Prompt, &edl, &recipe, &notes, &j.Dir)
	j.Created, j.Updated = time.Unix(created, 0), time.Unix(updated, 0)
	j.EDL, j.Recipe, j.Notes = json.RawMessage(edl), json.RawMessage(recipe), json.RawMessage(notes)
	return j, err
}

func (s *Store) Jobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobCols + ` FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) VisitorJobs(visitor string, limit int) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE visitor = ? ORDER BY created DESC LIMIT ?`, visitor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) DeleteJob(id string) error {
	if _, err := s.db.Exec(`DELETE FROM revisions WHERE job_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// Revision is one saved version of a job's edit.
type Revision struct {
	N, Parent   int
	EDL         json.RawMessage
	Instruction string
}

func (s *Store) AddRevision(jobID string, r Revision) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO revisions (job_id, n, parent, edl, instruction, created) VALUES (?,?,?,?,?,?)`,
		jobID, r.N, r.Parent, string(r.EDL), r.Instruction, time.Now().Unix())
	return err
}

var ErrNotFound = errors.New("not found")

func (s *Store) Revision(jobID string, n int) (Revision, error) {
	var r Revision
	var edl string
	err := s.db.QueryRow(`SELECT n, parent, edl, instruction FROM revisions WHERE job_id = ? AND n = ?`, jobID, n).
		Scan(&r.N, &r.Parent, &edl, &r.Instruction)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.EDL = json.RawMessage(edl)
	return r, err
}

// Prefs returns a visitor's saved preferences ("{}" if none) and marks them seen.
func (s *Store) Prefs(visitor string) (json.RawMessage, error) {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO visitors (id, created, seen) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET seen=excluded.seen`, visitor, now, now); err != nil {
		return nil, err
	}
	var p string
	if err := s.db.QueryRow(`SELECT prefs FROM visitors WHERE id = ?`, visitor).Scan(&p); err != nil {
		return nil, err
	}
	return json.RawMessage(p), nil
}

func (s *Store) SetPrefs(visitor string, prefs json.RawMessage) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO visitors (id, prefs, created, seen) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET prefs=excluded.prefs, seen=excluded.seen`, visitor, string(prefs), now, now)
	return err
}
