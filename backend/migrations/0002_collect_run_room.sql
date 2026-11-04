-- 0002 每轮归属：记录一轮内「房号 → 楼」的有效归属与跨楼冲突标记。
--
-- 这张表解决两个问题：
--   1. 断点续跑时从库里重建归属累计器。baseline 的 room 只保留全局最新归属，
--      且允许跨轮覆盖：同一房号在新一轮换了楼，room.building_id 就被新楼覆盖
--      （不报错、不拦截）。因此那一列只代表「最近一轮」，不能拿来续跑；
--      每轮一份快照才是「首个命中」的依据——轮内归属由本表加 Go 侧归属累计器
--      共同保证，一行写下后 building_id 不再改楼。
--   2. 发布阶段读当轮归属快照（release_room_building 的来源），不必回放响应。
--
-- 只落裁决后的有效归属（被丢弃的楼不在这里留行），冲突只在本行打标记：
-- conflict=true 表示该房号在本轮被别的楼也报过，conflict_building_id 记第一次
-- 被丢弃的楼；同一房号一轮至多一条冲突，与 collect_run.attr_conflict_count 的
-- 按房号计数口径一致。
CREATE TABLE collect_run_room (
  run_id               bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  term                 text NOT NULL,
  room_id              text NOT NULL REFERENCES room(room_id),
  building_id          text NOT NULL,
  conflict             boolean NOT NULL DEFAULT false,
  conflict_building_id text,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, room_id),
  -- 冲突标记与被丢弃的楼必须同进同出：标了冲突却没有楼、没冲突却带着楼，
  -- 都是无法判读的快照。续跑重建累计器时 Seed 对同样的组合直接报错，
  -- 落库侧在 Go 里也先挡了一道；这条 CHECK 是最后一道，防止绕过仓储的写入
  -- （运维补偿、手工修数据）落下「续跑永远读不回来」的行。
  -- 空串也要挡：Go 侧按 TrimSpace 后为空拒绝，DB 只判 IS NOT NULL 会放过 ''。
  CONSTRAINT collect_run_room_conflict_consistent CHECK (
    (conflict AND NULLIF(btrim(conflict_building_id), '') IS NOT NULL)
    OR (NOT conflict AND conflict_building_id IS NULL)
  )
);

-- 发布阶段按楼聚合当轮归属时走这条索引。
CREATE INDEX collect_run_room_building ON collect_run_room (run_id, building_id);
