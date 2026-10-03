-- 采集运行与告警记录。
-- 契约 docs/contract/db.v2.sql 未定义这三张表，依据是产品需求文档第 7.2 章
-- “运行状态与告警去重由后端与数据库维护”，以及第 9 章“只存请求参数、fetched_at
-- 与 page_sha256，不存原始 HTML”。这些表只服务面板展示与新鲜度判定，不参与观测语义。

-- 采集运行：每轮采集一行，面板据此显示运行状态
CREATE TABLE IF NOT EXISTS collect_run (
  run_id          bigserial PRIMARY KEY,
  started_at      timestamptz NOT NULL,
  finished_at     timestamptz,
  status          text NOT NULL,              -- running / success / failed / blocked
  term            text REFERENCES term(term), -- 还没解析出学期时为空
  weeks_total     integer NOT NULL DEFAULT 0,
  rooms_total     integer NOT NULL DEFAULT 0,
  unknown_cells   integer NOT NULL DEFAULT 0,
  composite_cells integer NOT NULL DEFAULT 0,
  release_id      text REFERENCES release(release_id), -- 本轮发布的版本，未发布时为空
  error_code      text,                       -- 便于告警去重与排查的分类码
  error_message   text,
  CHECK (status IN ('running', 'success', 'failed', 'blocked'))
);

-- 逐周采集结果：page_sha256 与 fetched_at 按契约只落在这里
CREATE TABLE IF NOT EXISTS collect_run_week (
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  term          text NOT NULL,
  week          smallint NOT NULL,
  ok            boolean NOT NULL,
  fetched_at    timestamptz,
  page_sha256   text,
  error_message text,
  PRIMARY KEY (run_id, term, week)
);

-- 告警去重：同一轮同类错误只通知一次，键里带轮次标识
CREATE TABLE IF NOT EXISTS alert_log (
  dedupe_key   text PRIMARY KEY,              -- 例如 retry_exhausted:20261003T041012+0800
  last_sent_at timestamptz NOT NULL,
  send_count   integer NOT NULL DEFAULT 1
);

-- 面板要取最近一轮运行状态，按开始时间倒序取第一条
CREATE INDEX IF NOT EXISTS collect_run_started_at
  ON collect_run (started_at DESC);
