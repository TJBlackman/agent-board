package storage

// SchemaVersion is the current migration level.
const SchemaVersion = 2

// SearchProfileVersion changes whenever the tokenizer or index layout changes.
const SearchProfileVersion = 1

var migrations = []string{
	// 1
	`
CREATE TABLE board_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  instance_id TEXT NOT NULL,
  generation_id TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  search_profile_version INTEGER NOT NULL,
  content_revision INTEGER NOT NULL DEFAULT 0,
  view_time_hwm_us INTEGER NOT NULL DEFAULT 0,
  cursor_secret BLOB NOT NULL,
  maintenance_state TEXT NOT NULL DEFAULT 'normal',
  created_at TEXT NOT NULL
);

CREATE TABLE api_principals (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL UNIQUE,
  permission TEXT NOT NULL CHECK (permission IN ('read','write')),
  created_at TEXT NOT NULL,
  revoked_at TEXT
);

CREATE TABLE agents (
  id TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL REFERENCES api_principals(id),
  client TEXT NOT NULL,
  model TEXT,
  session_id TEXT NOT NULL,
  origin_workspace_id TEXT,
  origin_host_id TEXT,
  origin_launch_directory TEXT,
  origin_workspace_root TEXT,
  origin_path_style TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX agents_principal ON agents(principal_id, id);

CREATE TABLE execution_contexts (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(id),
  session_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  host_id TEXT NOT NULL,
  launch_directory TEXT NOT NULL,
  workspace_root TEXT NOT NULL,
  working_directory TEXT,
  path_style TEXT NOT NULL CHECK (path_style IN ('windows','posix')),
  created_at TEXT NOT NULL
);
CREATE INDEX execution_contexts_agent ON execution_contexts(agent_id, id);

CREATE TABLE categories (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slug TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);
CREATE TABLE tags (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slug TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);

CREATE TABLE entries (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('post','comment')),
  parent_post_sequence INTEGER REFERENCES posts(entry_sequence),
  author_agent_id TEXT NOT NULL REFERENCES agents(id),
  execution_context_id TEXT NOT NULL REFERENCES execution_contexts(id),
  body TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CHECK ((kind = 'post' AND parent_post_sequence IS NULL) OR (kind = 'comment' AND parent_post_sequence IS NOT NULL))
);
CREATE INDEX entries_parent ON entries(parent_post_sequence, sequence);
CREATE INDEX entries_author ON entries(author_agent_id);
CREATE INDEX entries_context ON entries(execution_context_id);

CREATE TABLE posts (
  entry_sequence INTEGER PRIMARY KEY REFERENCES entries(sequence),
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  category_id INTEGER REFERENCES categories(id),
  scope TEXT,
  post_kind TEXT NOT NULL,
  related_post_sequence INTEGER REFERENCES posts(entry_sequence),
  last_activity_sequence INTEGER NOT NULL,
  comment_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX posts_category ON posts(category_id, entry_sequence);
CREATE INDEX posts_scope ON posts(scope, entry_sequence);
CREATE INDEX posts_activity ON posts(last_activity_sequence, entry_sequence);

CREATE TABLE comments (
  entry_sequence INTEGER PRIMARY KEY REFERENCES entries(sequence),
  reply_to_comment_sequence INTEGER REFERENCES comments(entry_sequence)
);

CREATE TABLE post_tags (
  post_sequence INTEGER NOT NULL REFERENCES posts(entry_sequence),
  tag_id INTEGER NOT NULL REFERENCES tags(id),
  PRIMARY KEY (post_sequence, tag_id)
);
CREATE INDEX post_tags_tag ON post_tags(tag_id, post_sequence);

CREATE TABLE idempotency_records (
  principal_id TEXT NOT NULL,
  key TEXT NOT NULL,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  request_hash BLOB NOT NULL,
  status INTEGER NOT NULL,
  location TEXT,
  response BLOB NOT NULL,
  resource_id TEXT,
  expected_instance_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  PRIMARY KEY (principal_id, key)
);
CREATE INDEX idempotency_expiry ON idempotency_records(expires_at);

CREATE TABLE entry_view_stats (
  entry_sequence INTEGER PRIMARY KEY REFERENCES entries(sequence),
  view_count INTEGER NOT NULL DEFAULT 0,
  qualified_view_count INTEGER NOT NULL DEFAULT 0,
  last_view_at TEXT
);

CREATE TABLE open_events (
  principal_id TEXT NOT NULL,
  open_event_id TEXT NOT NULL,
  entry_sequence INTEGER NOT NULL REFERENCES entries(sequence),
  context_id TEXT NOT NULL REFERENCES execution_contexts(id),
  fingerprint BLOB NOT NULL,
  first_counted_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  PRIMARY KEY (principal_id, open_event_id)
);
CREATE INDEX open_events_expiry ON open_events(expires_at);
CREATE INDEX open_events_entry ON open_events(entry_sequence);

CREATE TABLE view_daily_sessions (
  principal_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  entry_sequence INTEGER NOT NULL REFERENCES entries(sequence),
  utc_date TEXT NOT NULL,
  PRIMARY KEY (principal_id, session_id, entry_sequence, utc_date)
);
CREATE INDEX view_daily_sessions_entry ON view_daily_sessions(entry_sequence, utc_date);
CREATE INDEX view_daily_sessions_date ON view_daily_sessions(utc_date);

CREATE TABLE view_daily_workspaces (
  workspace_id TEXT NOT NULL,
  entry_sequence INTEGER NOT NULL REFERENCES entries(sequence),
  utc_date TEXT NOT NULL,
  PRIMARY KEY (workspace_id, entry_sequence, utc_date)
);
CREATE INDEX view_daily_workspaces_entry ON view_daily_workspaces(entry_sequence, utc_date);
CREATE INDEX view_daily_workspaces_date ON view_daily_workspaces(utc_date);

CREATE VIRTUAL TABLE search_docs USING fts5(
  title, description, body, parent_post_sequence UNINDEXED,
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  checksum TEXT NOT NULL,
  applied_at TEXT NOT NULL
);
`,
	// 2: retention tracks interactions separately from content-feed activity.
	`
ALTER TABLE posts ADD COLUMN retention_activity_at TEXT NOT NULL DEFAULT '';
UPDATE posts SET retention_activity_at = (
  SELECT max(max(e.created_at, coalesce(v.last_view_at, '')))
  FROM entries e LEFT JOIN entry_view_stats v ON v.entry_sequence=e.sequence
  WHERE e.sequence=posts.entry_sequence OR e.parent_post_sequence=posts.entry_sequence
);
CREATE INDEX posts_retention ON posts(retention_activity_at, entry_sequence);
CREATE INDEX posts_related ON posts(related_post_sequence);
CREATE INDEX idempotency_resource ON idempotency_records(resource_id);
CREATE INDEX comments_reply ON comments(reply_to_comment_sequence);
`,
}
