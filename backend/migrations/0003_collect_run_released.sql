-- 0003 发布标记：一轮只能发布一次，且不会被删旧版本重置。
--
-- 0001 只用 collect_run.release_id 记录本轮发布的版本，但该列是
-- `REFERENCES release(release_id) ON DELETE SET NULL`：CP8 删除更早版本时，
-- 指向它的轮次 release_id 会被置空，「一轮只能发布一次」的守卫因此失效——
-- 已被删版本覆盖的轮次可以再发布一次。新增的 released_at 独立记录「这一轮
-- 已经发布过」，外键动作不会改它；发布汇总的 UPDATE 以 released_at IS NULL
-- 作为守卫。
ALTER TABLE collect_run ADD COLUMN released_at timestamptz;

-- 回填：迁移前已经关联过版本的轮次视为已发布，避免升级后还能重发。
-- updated_at 是发布事务写下的时刻，用它近似发布时刻。
UPDATE collect_run SET released_at = updated_at WHERE release_id IS NOT NULL;

COMMENT ON COLUMN collect_run.released_at IS
  '发布时刻；NULL 表示这一轮尚未发布。非空即「一轮只能发布一次」的守卫，不随删旧版本重置';
