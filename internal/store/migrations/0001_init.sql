-- Links, soft-deletable. key is the normalized lookup name; only one live
-- (not deleted) link may hold a given key.
CREATE TABLE links (
  id          INTEGER PRIMARY KEY,
  name        TEXT    NOT NULL,
  key         TEXT    NOT NULL,
  target      TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT '',
  created_by  TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,          -- unix milliseconds
  updated_by  TEXT    NOT NULL,
  updated_at  INTEGER NOT NULL,
  rev         INTEGER NOT NULL DEFAULT 1, -- optimistic concurrency for edits
  clicks      INTEGER NOT NULL DEFAULT 0,
  last_click  INTEGER,
  deleted_at  INTEGER
);
CREATE UNIQUE INDEX links_live_key ON links(key) WHERE deleted_at IS NULL;
CREATE INDEX links_updated ON links(updated_at);

-- Append-only audit log. before/after hold JSON snapshots of the editable
-- fields ({"name","target","description"}).
CREATE TABLE link_events (
  id          INTEGER PRIMARY KEY,
  link_id     INTEGER NOT NULL REFERENCES links(id),
  action      TEXT    NOT NULL CHECK (action IN ('create','update','delete','restore','revert','import')),
  before_json TEXT,
  after_json  TEXT,
  actor_login TEXT    NOT NULL DEFAULT '',
  actor_node  TEXT    NOT NULL DEFAULT '',
  actor_ip    TEXT    NOT NULL,
  at          INTEGER NOT NULL
);
CREATE INDEX link_events_link ON link_events(link_id, id);

CREATE TRIGGER link_events_no_update BEFORE UPDATE ON link_events
BEGIN SELECT RAISE(ABORT, 'link_events is append-only'); END;

CREATE TRIGGER link_events_no_delete BEFORE DELETE ON link_events
BEGIN SELECT RAISE(ABORT, 'link_events is append-only'); END;

-- Links are never hard-deleted, so history always has a row to point at.
CREATE TRIGGER links_no_delete BEFORE DELETE ON links
BEGIN SELECT RAISE(ABORT, 'links are soft-deleted only'); END;
