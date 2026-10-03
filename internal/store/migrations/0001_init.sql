-- 契约 v2 的表结构：内容与 docs/contract/db.v2.sql 一致，改这里之前先改契约文件。
-- 约定：时间用 timestamptz；状态存语义键；不为省体积而拆键（可读优先）；表名不加前缀。

-- ---------- 字典与轴（版本化，属于数据本身） ----------

-- 状态字典：一个 version 是一份不可变的分类表
CREATE TABLE IF NOT EXISTS dict (
  version    integer PRIMARY KEY,
  note       text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dict_state (
  dict_version integer NOT NULL REFERENCES dict(version),
  state_key    text    NOT NULL,          -- 'free' / 'class' / 'unknown' ...
  label        text    NOT NULL,          -- 中文名
  available    boolean NOT NULL,          -- 是否算可用（空间意义上的空闲）
  PRIMARY KEY (dict_version, state_key)
);

-- 符号 → 状态键。分类规则本身也是数据，改表只影响之后的发布
CREATE TABLE IF NOT EXISTS dict_symbol (
  dict_version integer NOT NULL,
  symbol       text    NOT NULL,          -- 上游原文本，码点不得规范成 ASCII
  state_key    text    NOT NULL,
  PRIMARY KEY (dict_version, symbol),
  FOREIGN KEY (dict_version, state_key)
    REFERENCES dict_state(dict_version, state_key)
);

-- 节次轴：12 节点 ↔ 5 大节映射，版本化（作息表变化 = 新版本 + 阻断确认）
CREATE TABLE IF NOT EXISTS axis (
  version integer PRIMARY KEY,
  note    text
);

CREATE TABLE IF NOT EXISTS axis_node (
  axis_version  integer  NOT NULL REFERENCES axis(version),
  node          char(2)  NOT NULL,        -- '01'..'12'
  ordinal       smallint NOT NULL,        -- 展示顺序
  block         text     NOT NULL,        -- '0102' / '030405' ...
  block_ordinal smallint NOT NULL,
  PRIMARY KEY (axis_version, node),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- ---------- 学期与日历 ----------

CREATE TABLE IF NOT EXISTS term (
  term        text PRIMARY KEY,           -- '2025-2026-3'
  total_weeks smallint NOT NULL,
  timezone    text NOT NULL DEFAULT 'Asia/Shanghai'
);

-- 每周的周一绝对日期落库，消费方不再自己实现周次换算
CREATE TABLE IF NOT EXISTS term_week (
  term        text NOT NULL REFERENCES term(term) ON DELETE CASCADE,
  week        smallint NOT NULL,
  monday      date NOT NULL,
  in_calendar boolean NOT NULL DEFAULT true,
  PRIMARY KEY (term, week)
);

-- ---------- 房间目录 ----------

CREATE TABLE IF NOT EXISTS room (
  room_id    text PRIMARY KEY,            -- jsbh
  name       text NOT NULL,               -- 规范化展示名（最近一次）
  name_raw   text,                        -- 最近一次见到的原始名
  first_seen date,
  last_seen  date
);

-- ---------- WebUI 配置（管理员面板写入） ----------

CREATE TABLE IF NOT EXISTS settings (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- 已知键见 docs/contract/db.v2.sql：account_username / account_password /
-- feishu_webhook_url / feishu_secret / cron_expr / ocr_base_url。
-- account_password 与 feishu_secret 是秘密：不进 Git、日志、快照。
-- 管理员登录密码不在此表：用户名固定 admin，密码走环境变量 ADMIN_PASSWORD。

-- ---------- 发布 ----------

CREATE TABLE IF NOT EXISTS release (
  release_id   text PRIMARY KEY,          -- 可读短 id（时间戳字符串），无内容寻址
  term         text NOT NULL REFERENCES term(term),
  generated_at timestamptz NOT NULL,
  dict_version integer NOT NULL REFERENCES dict(version),
  axis_version integer NOT NULL REFERENCES axis(version),
  is_current   boolean NOT NULL DEFAULT false
);

-- 同一时刻至多一个 current
CREATE UNIQUE INDEX IF NOT EXISTS release_one_current
  ON release ((true)) WHERE is_current;

-- ---------- 观测（canonical 事实，每格一行） ----------

-- 一间房 × 一周 × 一天 × 一个节次恰一行。约 2079 × 7 × 12 ≈ 174,636 行/周。
-- term 冗余自 release（可读优先，便于单表直查）；state_key 存语义键；available 是
-- dict_state.available 在发布期的投影（dict 版本不可变，因此不会漂移），避免查询回连字典。
CREATE TABLE IF NOT EXISTS observation (
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  term       text     NOT NULL REFERENCES term(term),
  week       smallint NOT NULL,
  weekday    smallint NOT NULL,
  node       char(2)  NOT NULL,
  state_key  text     NOT NULL,
  available  boolean  NOT NULL,
  raw_text   text,                        -- 仅 unknown / composite / 非标准写法
  PRIMARY KEY (release_id, room_id, term, week, weekday, node),
  CHECK (weekday BETWEEN 1 AND 7),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- 空教室热路径：只扫可用格子
CREATE INDEX IF NOT EXISTS observation_free
  ON observation (release_id, term, week, weekday, node, room_id)
  WHERE available;
