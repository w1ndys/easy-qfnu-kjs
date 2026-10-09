-- 契约 v2 的 canonical 结构：清洗后的数据逐格入库。
-- 含义与不变量见 data-format.v2.md；查询契约见 api.v2.md。
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
  room_id       text PRIMARY KEY,         -- jsbh
  name          text NOT NULL,            -- 规范化展示名（最近一次）
  name_raw      text,                     -- 最近一次见到的原始名
  building_id   text,                     -- 返回该房间的 jxlbh；状态行本身不带楼，禁止从房名反推
  building_name text,                     -- 该次采集时的教学楼展示名
  first_seen    date,
  last_seen     date
);
-- building_id / building_name 尚未进入已执行的 internal/store/migrations/0001_init.sql。
-- 不要改 0001。采集器实现时另加迁移补上这两列。

-- ---------- WebUI 配置（管理员面板写入） ----------

CREATE TABLE IF NOT EXISTS settings (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- 已知键：
--   account_username / account_password   教务系统教师账号
--   feishu_webhook_url / feishu_secret   飞书告警（加签模式）
--   cron_expr                             采集调度（cron 表达式）
--   ocr_base_url                          ddddocr-fastapi 服务地址，例如 http://127.0.0.1:8000
--   building_whitelist                    教学楼白名单。JSON 数组，元素为 {"jxlbh","name"}。空数组不得触发全校请求。
-- account_password 与 feishu_secret 是秘密：不进 Git、日志、快照，由 WebUI 受控存取。
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

-- 保留策略：release 只保留 current 与 previous 两行（按 generated_at 取最近两条），更早的删除（观测级联删除）。

-- ---------- 常用查询形状（供实现参考，不是契约的一部分） ----------
-- 空教室：current release、给定 term/week/weekday、节点区间内每节可用，且房名命中关键词
--   SELECT o.room_id, r.name
--     FROM observation o
--     JOIN room r ON r.room_id = o.room_id
--    WHERE o.release_id = :current AND o.term = :term AND o.week = :week AND o.weekday = :weekday
--      AND o.node BETWEEN :start AND :end AND o.available
--      AND r.name ILIKE '%' || :keyword || '%'
--    GROUP BY o.room_id, r.name
--   HAVING count(*) = (:end::int - :start::int + 1)
--    ORDER BY r.name LIMIT :limit OFFSET :offset;

-- 版本对比：给定 current 与 previous 两个 release_id，按 (room_id, term, week, weekday, node) 求状态差集即可。
