package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNoSession means there is no live session for a token.
var ErrNoSession = errors.New("no such session")

// Session is a sign-in on the public address.
type Session struct {
	Provider string
	Subject  string
	Email    string
	Name     string
	Org      string
	IP       string
	Created  time.Time
	Expires  time.Time
}

// Session limits. Only allow-listed people can create sessions; these stop
// one of them (or a script with their account) from filling the table.
const (
	maxSessions           = 10000
	maxSessionsPerAccount = 10 // the oldest beyond this are signed out
)

// CreateSession stores a session under the SHA-256 of its token, first
// deleting expired sessions.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, x Session) error {
	if len(tokenHash) != 32 {
		return errors.New("session token hash must be 32 bytes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		return err
	}
	if n >= maxSessions {
		return errors.New("too many sessions; revoke some with `byname sessions revoke`")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(token_hash, provider, subject, email, name, org, ip, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, x.Provider, x.Subject, x.Email, x.Name, x.Org, x.IP,
		x.Created.UnixMilli(), x.Expires.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE provider = ? AND subject = ? AND token_hash NOT IN (
		SELECT token_hash FROM sessions WHERE provider = ? AND subject = ? ORDER BY created_at DESC, token_hash LIMIT ?)`,
		x.Provider, x.Subject, x.Provider, x.Subject, maxSessionsPerAccount); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionByHash returns the live session stored under tokenHash.
func (s *Store) SessionByHash(ctx context.Context, tokenHash []byte) (Session, error) {
	var x Session
	var created, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT provider, subject, email, name, org, ip, created_at, expires_at
		FROM sessions WHERE token_hash = ? AND expires_at > ?`, tokenHash, s.now().UnixMilli()).
		Scan(&x.Provider, &x.Subject, &x.Email, &x.Name, &x.Org, &x.IP, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	x.Created, x.Expires = time.UnixMilli(created), time.UnixMilli(expires)
	return x, nil
}

// DeleteSession removes the session stored under tokenHash, if any.
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// Sessions lists live sessions, newest first.
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, subject, email, name, org, ip, created_at, expires_at
		FROM sessions WHERE expires_at > ? ORDER BY created_at DESC`, s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		var created, expires int64
		if err := rows.Scan(&x.Provider, &x.Subject, &x.Email, &x.Name, &x.Org, &x.IP, &created, &expires); err != nil {
			return nil, err
		}
		x.Created, x.Expires = time.UnixMilli(created), time.UnixMilli(expires)
		out = append(out, x)
	}
	return out, rows.Err()
}

// RevokeSessions deletes every session for email, or every session at all
// when email is "". It returns how many it deleted, expired ones included.
func (s *Store) RevokeSessions(ctx context.Context, email string) (int64, error) {
	var res sql.Result
	var err error
	if email == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM sessions`)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE email = ?`, email)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
