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

-- ---------- WebUI 配置（管理员面板写入） ----------

-- 业务设置不再走环境变量；部署级变量（如 DATABASE_URL）仍走 env，见 docs/config/env.example。
-- account_password 与 feishu_secret 是秘密：不进 Git、日志、快照，由 WebUI 受控存取。
CREATE TABLE IF NOT EXISTS kjs.settings (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- 已知键：
--   account_username / account_password   教务系统教师账号
--   feishu_webhook_url / feishu_secret   飞书告警（加签模式）
--   cron_expr                             采集调度（cron 表达式）
--   ocr_base_url                          ddddocr-fastapi 服务地址，例如 http://127.0.0.1:8000

-- 采集锁、运行状态与发布验收不再依赖文件目录或外部地址：
--   锁：pg_advisory_lock（同一时刻至多一轮采集）；
--   运行状态/告警去重：落库（结果在 kjs.release_week、kjs.release_check；去重键由服务进程维护，实现 Spec 定）。

-- ---------- 房间目录 ----------

-- 身份与属性，跨发布复用；全量房间，不按白名单裁剪
CREATE TABLE IF NOT EXISTS kjs.room (
  room_id    text PRIMARY KEY,            -- jsbh
  name       text NOT NULL,               -- 规范化展示名（最近一次）
  name_raw   text,                        -- 最近一次见到的原始名
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
  is_current     boolean NOT NULL DEFAULT false,
  previous_id    text
);

-- 同一时刻至多一个 current
CREATE UNIQUE INDEX IF NOT EXISTS kjs_release_one_current
  ON kjs.release ((true)) WHERE is_current;


-- 房间在某次发布中的呈现快照
CREATE TABLE IF NOT EXISTS kjs.release_room (
  release_id text NOT NULL REFERENCES kjs.release(release_id) ON DELETE CASCADE,
  room_id    text NOT NULL,
  name       text NOT NULL,
  name_raw   text,
  PRIMARY KEY (release_id, room_id)
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
-- 空教室：给定 release、周、星期、节点区间，返回区间内每一节都可用的房间；
-- 关键词用 kjs.release_room.name 的子串匹配（NFKC、ASCII 不区分大小写）。
--   SELECT o.room_id
--     FROM kjs.observation o
--     JOIN kjs.release_room r ON r.release_id = o.release_id AND r.room_id = o.room_id
--    WHERE o.release_id = $1
--      AND o.week = $2 AND o.weekday = $3
--      AND o.node BETWEEN $4 AND $5
--      AND o.available
--      AND r.name ILIKE '%' || $6 || '%'
--    GROUP BY o.room_id
--   HAVING count(*) = ($5::int - $4::int + 1);
