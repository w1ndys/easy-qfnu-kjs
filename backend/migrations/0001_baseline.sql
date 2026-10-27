-- 0001 基线：字典与轴、学期日历、教室、管理员与设置、发布版本、观测与明细、
-- 采集轮次与任务、访问统计、告警。全部对象与约束见 design.md「数据模型」。

-- 字典与轴：版本化，采集与查询都按字典投影可用性。
CREATE TABLE dict (
  version    integer PRIMARY KEY,
  note       text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dict_state (
  dict_version integer NOT NULL REFERENCES dict(version),
  state_key    text    NOT NULL,
  label        text    NOT NULL,
  available    boolean NOT NULL,
  PRIMARY KEY (dict_version, state_key)
);

CREATE TABLE dict_symbol (
  dict_version integer NOT NULL,
  symbol       text    NOT NULL,
  state_key    text    NOT NULL,
  PRIMARY KEY (dict_version, symbol),
  FOREIGN KEY (dict_version, state_key) REFERENCES dict_state(dict_version, state_key)
);

CREATE TABLE axis (
  version integer PRIMARY KEY,
  note    text
);

CREATE TABLE axis_node (
  axis_version  integer  NOT NULL REFERENCES axis(version),
  node          char(2)  NOT NULL,
  ordinal       smallint NOT NULL,
  block         text     NOT NULL,
  block_ordinal smallint NOT NULL,
  PRIMARY KEY (axis_version, node),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- 学期日历：覆盖整个学期，仅矩阵采集受窗口限制。
CREATE TABLE term (
  term        text PRIMARY KEY,
  total_weeks smallint NOT NULL,
  timezone    text NOT NULL DEFAULT 'Asia/Shanghai'
);

CREATE TABLE term_week (
  term        text NOT NULL REFERENCES term(term) ON DELETE CASCADE,
  week        smallint NOT NULL,
  monday      date NOT NULL,
  in_calendar boolean NOT NULL DEFAULT true,
  PRIMARY KEY (term, week)
);

-- 教室目录：全局最新归属，供异常视图与目录使用；查询走版本快照。
CREATE TABLE room (
  room_id       text PRIMARY KEY,
  name          text NOT NULL,
  name_raw      text,
  building_id   text,
  building_name text,
  first_seen    date,
  last_seen     date
);

-- 管理员：单账号，只存不可逆哈希。
CREATE TABLE admin_user (
  id                  smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  password_hash       text NOT NULL,
  password_updated_at timestamptz NOT NULL DEFAULT now(),
  created_at          timestamptz NOT NULL DEFAULT now()
);

-- 业务设置：密钥材料与秘密值都以密文形态存放。
CREATE TABLE settings (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  is_secret  boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- 发布版本：只保留 current 与 previous。
CREATE TABLE release (
  release_id        text PRIMARY KEY,
  term              text NOT NULL REFERENCES term(term),
  generated_at      timestamptz NOT NULL,
  dict_version      integer NOT NULL REFERENCES dict(version),
  axis_version      integer NOT NULL REFERENCES axis(version),
  has_attr_conflict boolean NOT NULL DEFAULT false,
  is_current        boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX release_one_current ON release ((true)) WHERE is_current;

-- 归属快照：随版本切换，回退时一起回退。
CREATE TABLE release_room_building (
  release_id   text NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id      text NOT NULL REFERENCES room(room_id),
  building_id  text NOT NULL,
  building_name text NOT NULL,
  PRIMARY KEY (release_id, room_id)
);

CREATE INDEX release_room_building_bld ON release_room_building (release_id, building_id);

-- 逐格观测：一间房 × 一周 × 一天 × 一节恰一行。
CREATE TABLE observation (
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  term       text     NOT NULL REFERENCES term(term),
  week       smallint NOT NULL,
  weekday    smallint NOT NULL,
  node       char(2)  NOT NULL,
  state_key  text     NOT NULL,
  available  boolean  NOT NULL,
  raw_text   text,
  PRIMARY KEY (release_id, room_id, term, week, weekday, node),
  CHECK (weekday BETWEEN 1 AND 7)
);

CREATE INDEX observation_free ON observation (release_id, term, week, weekday, node, room_id) WHERE available;

-- 占用说明：只保留教室状态、课程、周次范围与时间标志。
CREATE TABLE occupancy_detail (
  detail_id  bigserial PRIMARY KEY,
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  weekday    smallint NOT NULL,
  block      text     NOT NULL,
  state_key  text     NOT NULL,
  course     text,
  week_range text,
  time_flag  text,
  derived    boolean  NOT NULL DEFAULT false,
  CHECK (weekday BETWEEN 1 AND 7)
);

CREATE INDEX occupancy_detail_cell ON occupancy_detail (release_id, room_id, weekday, block);

-- 采集轮次：同一时刻至多一轮 running，由部分唯一索引保证。
CREATE TABLE collect_run (
  run_id             bigserial PRIMARY KEY,
  started_at         timestamptz NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz,
  status             text NOT NULL CHECK (status IN ('running','success','partial','failed','blocked','skipped')),
  phase              text NOT NULL DEFAULT 'login' CHECK (phase IN ('login','calendar','week_matrix','cell_detail','validate','publish','finished')),
  term               text REFERENCES term(term),
  window_start       smallint,
  window_end         smallint,
  weeks_total        integer NOT NULL DEFAULT 0,
  rooms_total        integer NOT NULL DEFAULT 0,
  required_total     integer NOT NULL DEFAULT 0,
  required_done      integer NOT NULL DEFAULT 0,
  optional_total     integer NOT NULL DEFAULT 0,
  optional_done      integer NOT NULL DEFAULT 0,
  unknown_cells      integer NOT NULL DEFAULT 0,
  composite_cells    integer NOT NULL DEFAULT 0,
  attr_conflict_count integer NOT NULL DEFAULT 0,
  release_id         text REFERENCES release(release_id) ON DELETE SET NULL,
  error_code         text,
  error_message      text
);

CREATE UNIQUE INDEX collect_run_single_running ON collect_run ((true)) WHERE status = 'running';
CREATE INDEX collect_run_started_at ON collect_run (started_at DESC);

-- 每轮每楼每周的抓取结果，含页面哈希便于判断上游是否变化。
CREATE TABLE collect_run_week (
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  term          text NOT NULL,
  week          smallint NOT NULL,
  building_id   text NOT NULL,
  ok            boolean NOT NULL,
  fetched_at    timestamptz,
  page_sha256   text,
  error_message text,
  PRIMARY KEY (run_id, term, week, building_id)
);

-- 同步任务：周矩阵必需、占用说明可选，断点续传靠 status。
CREATE TABLE sync_task (
  task_id       bigserial PRIMARY KEY,
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('week_matrix','cell_detail')),
  required      boolean NOT NULL,
  building_id   text NOT NULL,
  week          smallint,
  room_id       text REFERENCES room(room_id),
  weekday       smallint,
  block         text,
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','done','failed')),
  attempt_count integer NOT NULL DEFAULT 0,
  error_message text,
  CHECK ((kind = 'week_matrix' AND week IS NOT NULL AND room_id IS NULL AND weekday IS NULL AND block IS NULL)
      OR (kind = 'cell_detail' AND week IS NULL AND room_id IS NOT NULL AND weekday IS NOT NULL AND block IS NOT NULL))
);

CREATE UNIQUE INDEX sync_task_week ON sync_task (run_id, building_id, week) WHERE kind = 'week_matrix';
CREATE UNIQUE INDEX sync_task_cell ON sync_task (run_id, building_id, room_id, weekday, block) WHERE kind = 'cell_detail';
CREATE INDEX sync_task_run_status ON sync_task (run_id, status);

-- 访问统计：只存小时级计数与 UV 去重摘要，保留 90 天。
CREATE TABLE stat_hour (
  day     date     NOT NULL,
  hour    smallint NOT NULL,
  weekday smallint NOT NULL,
  pv      integer  NOT NULL DEFAULT 0,
  PRIMARY KEY (day, hour)
);

CREATE TABLE stat_visitor (
  day          date     NOT NULL,
  hour         smallint NOT NULL,
  visitor_hash text     NOT NULL,
  PRIMARY KEY (day, hour, visitor_hash)
);

CREATE TABLE stat_building_day (
  day         date    NOT NULL,
  building_id text    NOT NULL,
  hits        integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, building_id)
);

CREATE TABLE stat_room_day (
  day     date    NOT NULL,
  room_id text    NOT NULL,
  hits    integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, room_id)
);

CREATE TABLE stat_keyword_day (
  day     date    NOT NULL,
  keyword text    NOT NULL,
  hits    integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, keyword)
);

CREATE TABLE stat_node_day (
  day  date    NOT NULL,
  node char(2) NOT NULL,
  hits integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, node)
);

-- 告警去重与迁移记录。
CREATE TABLE alert_log (
  dedupe_key   text PRIMARY KEY,
  last_sent_at timestamptz NOT NULL,
  send_count   integer NOT NULL DEFAULT 1
);

-- 迁移执行器会先建好这张表，所以基线里同样用 IF NOT EXISTS，两条路径都成立。
CREATE TABLE IF NOT EXISTS schema_migration (
  version    text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);

-- 种子：字典 v1（完全空闲只作展示标签，不进符号表）、轴 v1、默认设置。
INSERT INTO dict (version, note) VALUES (1, '契约种子');

INSERT INTO dict_state (dict_version, state_key, label, available) VALUES
  (1, 'class',            '正常上课',   false),
  (1, 'borrowed',         '借用',       false),
  (1, 'locked',           '锁定',       false),
  (1, 'exam',             '考试',       false),
  (1, 'free',             '空闲',       true),
  (1, 'fixed_reschedule', '固定调课',   false),
  (1, 'temp_reschedule',  '临时调课',   false),
  (1, 'fully_free',       '完全空闲',   true),
  (1, 'cross_mode',       '跨模式占用', false),
  (1, 'composite',        '复合占用',   false),
  (1, 'unknown',          '未知',       false);

INSERT INTO dict_symbol (dict_version, symbol, state_key) VALUES
  (1, '◆', 'class'),
  (1, 'Ｊ', 'borrowed'),
  (1, 'Ｘ', 'locked'),
  (1, 'Κ', 'exam'),
  (1, '空闲', 'free'),
  (1, 'Ｇ', 'fixed_reschedule'),
  (1, 'Ｌ', 'temp_reschedule'),
  (1, 'M', 'cross_mode');

INSERT INTO axis (version, note) VALUES (1, '5 大节 → 12 小节');

INSERT INTO axis_node (axis_version, node, ordinal, block, block_ordinal) VALUES
  (1, '01', 1,  '0102',    1),
  (1, '02', 2,  '0102',    1),
  (1, '03', 3,  '030405',  2),
  (1, '04', 4,  '030405',  2),
  (1, '05', 5,  '030405',  2),
  (1, '06', 6,  '0607',    3),
  (1, '07', 7,  '0607',    3),
  (1, '08', 8,  '0809',    4),
  (1, '09', 9,  '0809',    4),
  (1, '10', 10, '101112',  5),
  (1, '11', 11, '101112',  5),
  (1, '12', 12, '101112',  5);

INSERT INTO settings (key, value) VALUES
  ('cron_expr', '0 10 4 * * 6'),
  ('building_whitelist', '[]');
