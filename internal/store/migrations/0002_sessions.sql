-- Sign-in sessions for the public (Tailscale Funnel) address. The browser's
-- cookie holds a random token; only its SHA-256 is stored, so a copy of the
-- database (a snapshot, an S3 backup) can't be used to sign in.
CREATE TABLE sessions (
  token_hash BLOB    PRIMARY KEY,
  provider   TEXT    NOT NULL,           -- google | slack
  subject    TEXT    NOT NULL,           -- the provider's user ID
  email      TEXT    NOT NULL,
  name       TEXT    NOT NULL DEFAULT '',
  org        TEXT    NOT NULL DEFAULT '', -- Google Workspace domain or Slack workspace ID
  ip         TEXT    NOT NULL DEFAULT '', -- address that signed in
  created_at INTEGER NOT NULL,           -- unix milliseconds
  expires_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX sessions_expires ON sessions(expires_at);
CREATE INDEX sessions_email ON sessions(email);
