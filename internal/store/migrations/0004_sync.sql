-- 采集器全量同步用到的结构：房间所属楼、同步任务与占用明细。
-- 依据 specs/collector-full-sync/design.md 的 Data Models、docs/contract/db.v2.sql（room 的
-- building_id / building_name 早已写进契约，但没进 0001，契约第 76 行要求另加迁移补上），
-- 与 docs/decisions/2026-10-09-saturday-full-sync.md。
-- sync_task 与 occupancy_detail 不在 docs/contract/db.v2.sql 里：和 0002 的运行记录表一样，
-- 它们只服务采集进度与格子说明，不参与观测语义。

-- ---------- 房间所属楼 ----------

-- 房间的楼由返回它的那次按楼请求决定，状态行本身不带楼，禁止从房名反推。
-- 两列可空：0004 之前入库的房间还没有楼，采集器下一轮写入时补上。
ALTER TABLE room ADD COLUMN IF NOT EXISTS building_id   text;
ALTER TABLE room ADD COLUMN IF NOT EXISTS building_name text;

-- ---------- 同步任务 ----------

-- 一轮全量里的一个可续传单元，粒度是「楼 + 周矩阵」或「楼 + 房间 + 格子明细」。
-- 已完成的任务不重打：状态机从第一个 status = 'pending' 的行继续。
CREATE TABLE IF NOT EXISTS sync_task (
  task_id       bigserial PRIMARY KEY,
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  kind          text   NOT NULL,           -- week_matrix / cell_detail
  building_id   text   NOT NULL,           -- 该任务请求的 jxlbh
  week          smallint,                  -- 周矩阵任务的周次；明细任务为空
  room_id       text   REFERENCES room(room_id),
  weekday       smallint,                  -- 明细任务的星期，1=周一 … 7=周日
  block         text,                      -- 明细任务的大节编码，取值见 axis_node.block
  status        text   NOT NULL DEFAULT 'pending',
  attempt_count integer NOT NULL DEFAULT 0, -- 上游重试次数，面板据此看哪些任务反复失败
  error_message text,
  CHECK (kind IN ('week_matrix', 'cell_detail')),
  CHECK (status IN ('pending', 'done', 'failed')),
  CHECK (attempt_count >= 0),
  CHECK (weekday IS NULL OR weekday BETWEEN 1 AND 7),
  -- 两类任务各有固定的列形状：周矩阵只带楼与周次，明细必须能定位到一个格子。
  -- 形状由约束固定，避免状态机取到一个定位不了格子的续传单元。
  CHECK (
    (kind = 'week_matrix' AND week IS NOT NULL AND room_id IS NULL     AND weekday IS NULL     AND block IS NULL)
    OR
    (kind = 'cell_detail' AND week IS NULL     AND room_id IS NOT NULL AND weekday IS NOT NULL AND block IS NOT NULL)
  )
);

-- 同一轮里同一栋楼的同一周只该有一个周矩阵任务，重复建任务靠它幂等
CREATE UNIQUE INDEX IF NOT EXISTS sync_task_week_matrix
  ON sync_task (run_id, building_id, week)
  WHERE kind = 'week_matrix';

-- 同一轮里同一栋楼的同一个格子只该有一个明细任务
CREATE UNIQUE INDEX IF NOT EXISTS sync_task_cell_detail
  ON sync_task (run_id, building_id, room_id, weekday, block)
  WHERE kind = 'cell_detail';

-- 面板看进度、断点续传找第一个未完成任务，都按轮次取
CREATE INDEX IF NOT EXISTS sync_task_run_status
  ON sync_task (run_id, status);

-- ---------- 占用明细 ----------

-- 一个「房间 × 星期 × 大节」格子的占用说明：只存教室状态、课程、周次范围与时间标志。
-- 不存申请人，不存任课教师，不存原始 HTML（见 docs/product-requirements.md 数据范围一节）。
-- 同一格子可能有多条说明（课程与考试科目分列），所以键不落在业务列上。
CREATE TABLE IF NOT EXISTS occupancy_detail (
  detail_id  bigserial PRIMARY KEY,
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  weekday    smallint NOT NULL,   -- 1=周一 … 7=周日
  block      text     NOT NULL,   -- 大节编码，取值见 axis_node.block
  state_key  text     NOT NULL,   -- 教室状态语义键，含义由 release 的 dict_version 决定
  course     text,                -- 课程名；考试科目出现在课程字段时同样落在这里
  week_range text,                -- 明细返回的周次范围原文，解析交给派生逻辑
  time_flag  text,                -- 时间标志（单双周等）原文
  derived    boolean  NOT NULL DEFAULT false, -- 该说明是否只用于没有周矩阵的周
  CHECK (weekday BETWEEN 1 AND 7)
);

-- 查询与派生都按「版本 + 房间 + 星期 + 大节」取说明
CREATE INDEX IF NOT EXISTS occupancy_detail_cell
  ON occupancy_detail (release_id, room_id, weekday, block);
