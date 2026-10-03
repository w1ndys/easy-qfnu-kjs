-- 契约 v2 的 canonical 结构：清洗后的数据直接入这些表。
-- 含义与不变量见 data-format.v2.md；本文件是形态。
-- 约定：schema 固定 kjs；时间用 timestamptz；状态一律存语义键，不存整数。

CREATE SCHEMA IF NOT EXISTS kjs;

-- ---------- 字典与轴（版本化，属于数据本身） ----------

-- 状态字典：一个 version 是一份不可变的分类表
CREATE TABLE IF NOT EXISTS kjs.dict (
  version    integer PRIMARY KEY,
  note       text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS kjs.dict_state (
  dict_version integer NOT NULL REFERENCES kjs.dict(version),
  state_key    text    NOT NULL,          -- 'free' / 'class' / 'unknown' ...
  label        text    NOT NULL,          -- 中文名
  available    boolean NOT NULL,          -- 是否算可用（空间意义上的空闲）
  PRIMARY KEY (dict_version, state_key)
);

-- 符号 → 状态键。分类规则本身也是数据，改表只影响之后的发布
CREATE TABLE IF NOT EXISTS kjs.dict_symbol (
  dict_version integer NOT NULL,
  symbol       text    NOT NULL,          -- 上游原文本，码点不得规范成 ASCII
  state_key    text    NOT NULL,
  PRIMARY KEY (dict_version, symbol),
  FOREIGN KEY (dict_version, state_key)
    REFERENCES kjs.dict_state(dict_version, state_key)
);

-- 节次轴：12 节点 ↔ 5 大节映射，版本化（作息表变化 = 新版本 + 阻断确认）
CREATE TABLE IF NOT EXISTS kjs.axis (
  version integer PRIMARY KEY,
  note    text
);

CREATE TABLE IF NOT EXISTS kjs.axis_node (
  axis_version  integer  NOT NULL REFERENCES kjs.axis(version),
  node          char(2)  NOT NULL,        -- '01'..'12'
  ordinal       smallint NOT NULL,        -- 展示顺序
  block         text     NOT NULL,        -- '0102' / '030405' ...
  block_ordinal smallint NOT NULL,
  PRIMARY KEY (axis_version, node),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- ---------- 学期与日历 ----------

CREATE TABLE IF NOT EXISTS kjs.term (
  term        text PRIMARY KEY,           -- '2025-2026-3'
  total_weeks smallint NOT NULL,
  timezone    text NOT NULL DEFAULT 'Asia/Shanghai'
);

-- 每周的周一绝对日期落库，消费方不再自己实现周次换算
CREATE TABLE IF NOT EXISTS kjs.term_week (
  term        text NOT NULL REFERENCES kjs.term(term) ON DELETE CASCADE,
  week        smallint NOT NULL,
  monday      date NOT NULL,
  in_calendar boolean NOT NULL DEFAULT true,
  PRIMARY KEY (term, week)
);

-- ---------- 房间目录 ----------

-- 身份与属性，跨发布复用；全量房间，不按白名单裁剪
CREATE TABLE IF NOT EXISTS kjs.room (
  room_id    text PRIMARY KEY,            -- jsbh
  name       text NOT NULL,               -- 规范化展示名（最近一次）
  name_raw   text,                        -- 最近一次见到的原始名
  capacity   smallint,                    -- 从 (n/N) 解出，可为空
  first_seen date,
  last_seen  date
);

-- ---------- 发布 ----------

-- 一份不可变的观测集合，内容寻址
CREATE TABLE IF NOT EXISTS kjs.release (
  release_id     text PRIMARY KEY,        -- 'sha256:...'
  term           text NOT NULL REFERENCES kjs.term(term),
  generated_at   timestamptz NOT NULL,
  dict_version   integer NOT NULL REFERENCES kjs.dict(version),
  axis_version   integer NOT NULL REFERENCES kjs.axis(version),
  config_version text NOT NULL,           -- 白名单规则的 hash
  is_current     boolean NOT NULL DEFAULT false,
  previous_id    text
);

-- 同一时刻至多一个 current
CREATE UNIQUE INDEX IF NOT EXISTS kjs_release_one_current
  ON kjs.release ((true)) WHERE is_current;

-- 分组视图：来自人工维护的白名单；零命中是告警，不是失败
CREATE TABLE IF NOT EXISTS kjs.release_group (
  release_id text NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  group_id   text NOT NULL,
  name       text NOT NULL,
  ordinal    smallint,
  room_count integer NOT NULL,
  PRIMARY KEY (release_id, group_id)
);

-- 房间在某次发布中的呈现快照
CREATE TABLE IF NOT EXISTS kjs.release_room (
  release_id text NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  room_id    text NOT NULL,
  name       text NOT NULL,
  name_raw   text,
  capacity   smallint,
  PRIMARY KEY (release_id, room_id)
);

-- 多对多：一间房可以同时属于多个视图
CREATE TABLE IF NOT EXISTS kjs.release_room_group (
  release_id text NOT NULL,
  room_id    text NOT NULL,
  group_id   text NOT NULL,
  PRIMARY KEY (release_id, room_id, group_id),
  FOREIGN KEY (release_id, room_id)
    REFERENCES kjs.release_room(release_id, room_id) ON DELETE CASCADE,
  FOREIGN KEY (release_id, group_id)
    REFERENCES kjs.release_group(release_id, group_id) ON DELETE CASCADE
);

-- ---------- 观测（canonical 事实） ----------

-- 一间房 × 一周 × 一天 × 一个节次恰一行。约 2079 × 7 × 12 ≈ 174,636 行/周。
-- available 是 dict_state.available 在发布期的投影（dict 版本不可变，因此不会漂移），
-- 存下来是为了让空教室查询不必回连字典表。
CREATE TABLE IF NOT EXISTS kjs.observation (
  release_id text     NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL,
  week       smallint NOT NULL,
  weekday    smallint NOT NULL,
  node       char(2)  NOT NULL,
  state_key  text     NOT NULL,
  available  boolean  NOT NULL,
  raw_text   text,                        -- 仅 unknown / composite / 非标准写法
  PRIMARY KEY (release_id, room_id, week, weekday, node),
  CHECK (weekday BETWEEN 1 AND 7),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- 主查询路径：(release, 周, 星期, 节次) → 房间
CREATE INDEX IF NOT EXISTS kjs_observation_query
  ON kjs.observation (release_id, week, weekday, node, room_id);

-- 空教室的热路径：只扫可用格子
CREATE INDEX IF NOT EXISTS kjs_observation_free
  ON kjs.observation (release_id, week, weekday, node, room_id)
  WHERE available;

-- ---------- 逐周状态、来源与校验 ----------

CREATE TABLE IF NOT EXISTS kjs.release_week (
  release_id      text NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  week            smallint NOT NULL,
  content_hash    text NOT NULL,
  last_success_at timestamptz,
  last_attempt_at timestamptz,
  last_error_code text,
  row_count       integer,
  unknown_cells   integer NOT NULL DEFAULT 0,
  composite_cells integer NOT NULL DEFAULT 0,
  PRIMARY KEY (release_id, week)
);

-- 只存请求参数与页面哈希，不存原始 HTML
CREATE TABLE IF NOT EXISTS kjs.release_week_source (
  release_id  text NOT NULL,
  week        smallint NOT NULL,
  endpoint    text NOT NULL,
  params      jsonb NOT NULL,
  fetched_at  timestamptz NOT NULL,
  page_sha256 text,
  PRIMARY KEY (release_id, week),
  FOREIGN KEY (release_id, week)
    REFERENCES kjs.release_week(release_id, week) ON DELETE CASCADE
);

-- 校验结果即数据：人工与前端都能看见这一版检查了什么
CREATE TABLE IF NOT EXISTS kjs.release_check (
  release_id text NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  check_id   text NOT NULL,
  status     text NOT NULL,               -- pass / warn / fail
  detail     text,
  PRIMARY KEY (release_id, check_id)
);

-- 保留策略：只留 current 与 previous 两份 release，更早的删除（子表级联删除）。

-- ---------- 常用查询形状（供实现参考，不是契约的一部分） ----------
-- 空教室：给定 release、视图、周、星期、节点区间，返回区间内每一节都可用的房间
--   SELECT o.room_id
--     FROM kjs.observation o
--     JOIN kjs.release_room_group g
--       ON g.release_id = o.release_id AND g.room_id = o.room_id
--    WHERE o.release_id = $1 AND g.group_id = $2
--      AND o.week = $3 AND o.weekday = $4
--      AND o.node BETWEEN $5 AND $6
--      AND o.available
--    GROUP BY o.room_id
--   HAVING count(*) = ($6::int - $5::int + 1);
