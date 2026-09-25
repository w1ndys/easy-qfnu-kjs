CREATE SCHEMA IF NOT EXISTS kjs;

CREATE TABLE IF NOT EXISTS kjs.release (
  release_id text PRIMARY KEY,
  term text NOT NULL,
  generated_at timestamptz NOT NULL,
  anchor_date date NOT NULL,
  anchor_week integer NOT NULL,
  total_weeks integer NOT NULL,
  timezone text NOT NULL,
  in_teaching_calendar boolean NOT NULL,
  is_current boolean NOT NULL DEFAULT false,
  manifest_body text NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS kjs_release_one_current
  ON kjs.release ((true))
  WHERE is_current;

CREATE TABLE IF NOT EXISTS kjs.release_group (
  release_id text NOT NULL REFERENCES kjs.release (release_id) ON DELETE CASCADE,
  group_id text NOT NULL,
  name text NOT NULL,
  sort_order integer NOT NULL,
  room_count integer NOT NULL,
  PRIMARY KEY (release_id, group_id)
);

CREATE TABLE IF NOT EXISTS kjs.release_week (
  release_id text NOT NULL REFERENCES kjs.release (release_id) ON DELETE CASCADE,
  week integer NOT NULL,
  snapshot_id text NOT NULL,
  sha256 text NOT NULL,
  generated_at timestamptz NOT NULL,
  last_success_at timestamptz NOT NULL,
  last_error_code text,
  last_attempt_at timestamptz,
  body text NOT NULL,
  PRIMARY KEY (release_id, week)
);

CREATE TABLE IF NOT EXISTS kjs.room_slot (
  release_id text NOT NULL,
  week integer NOT NULL,
  day integer NOT NULL,
  jsbh text NOT NULL,
  group_id text NOT NULL,
  name text NOT NULL,
  node text NOT NULL,
  status integer NOT NULL,
  PRIMARY KEY (release_id, week, day, jsbh, node),
  FOREIGN KEY (release_id, week) REFERENCES kjs.release_week (release_id, week) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS kjs_room_slot_query
  ON kjs.room_slot (release_id, week, day, group_id, node);
