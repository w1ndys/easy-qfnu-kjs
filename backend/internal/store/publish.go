package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// RunFacts 是一轮采集在 collect_run 里供发布组装读取的事实：
// 学期、采集窗口与归属冲突计数。
//
// 字典版本与轴版本不在这里：collect_run 没有这两列，它们由采集编排在清洗时
// 使用的字典/轴快照带进发布组装的入参（见 publish.RunInput）。
// 窗口允许为 NULL（还没定窗口的轮次），读回时折成 0，由 publish.Build 判非法。
type RunFacts struct {
	RunID             int64  // 轮次编号
	Term              string // 学期；轮次未写学期时为空串
	WindowStart       int    // 采集窗口起始周；NULL 读回 0
	WindowEnd         int    // 采集窗口结束周；NULL 读回 0
	AttrConflictCount int    // 本轮归属冲突数，决定版本的 has_attr_conflict 标记
}

// publishQuerier 是发布阶段只读仓储需要的最小能力：它由两个既有窄接口组合而来，
// 归属快照从 room.go 读、任务汇总从 runphase.go 读，本文件只新增 RunFacts 一条查询。
// 组合而不是重写方法声明，是为了让 PublishRepo 能直接把同一个查询器交给既有函数，
// 不复制它们的 SQL 与错误口径。
type publishQuerier interface {
	roomQuerier
	runPhaseQuerier
}

// loadRunFactsSQL 读一轮的学期、窗口与归属冲突计数。
// term/window 允许 NULL：这里折成空串与 0，由调用方判「轮次缺学期 / 没定窗口」，
// 不让扫描 NULL 失败掩盖真正的数据缺口。
const loadRunFactsSQL = `
SELECT run_id,
       COALESCE(term, ''),
       COALESCE(window_start, 0),
       COALESCE(window_end, 0),
       attr_conflict_count
  FROM collect_run
 WHERE run_id = $1`

// LoadRunFacts 读一轮的发布相关事实。轮次不存在时返回包装 ErrRunStoreFailed 的
// 可读错误，不把零值当成「窗口为 0 的合法轮次」。
func LoadRunFacts(ctx context.Context, q publishQuerier, runID int64) (RunFacts, error) {
	var facts RunFacts
	err := q.QueryRow(ctx, loadRunFactsSQL, runID).Scan(
		&facts.RunID, &facts.Term, &facts.WindowStart, &facts.WindowEnd, &facts.AttrConflictCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunFacts{}, fmt.Errorf("%w: 轮次 %d 不存在，无法读取发布事实", model.ErrRunStoreFailed, runID)
	}
	if err != nil {
		return RunFacts{}, fmt.Errorf("%w: 读取轮次 %d 发布事实失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	return facts, nil
}

// PublishRepo 把查询器闭在内部，供 publish 以窄接口消费：
// publish 只看到「轮次事实、归属快照、任务汇总」三个读方法，不接触 pgx、SQL
// 与 collect_run / collect_run_room / sync_task 的列细节（依赖方向见
// backend/.golangci.yml 的 publish 规则）。
type PublishRepo struct {
	q publishQuerier
}

// NewPublishRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewPublishRepo(q publishQuerier) PublishRepo {
	return PublishRepo{q: q}
}

// LoadRunFacts 见同名函数。
func (r PublishRepo) LoadRunFacts(ctx context.Context, runID int64) (RunFacts, error) {
	return LoadRunFacts(ctx, r.q, runID)
}

// LoadRunRooms 读当轮归属快照，见同名函数。
func (r PublishRepo) LoadRunRooms(ctx context.Context, runID int64) ([]RunRoom, error) {
	return LoadRunRooms(ctx, r.q, runID)
}

// LoadRunTaskSummary 聚合当轮必需/可选任务完成情况，见同名函数。
func (r PublishRepo) LoadRunTaskSummary(ctx context.Context, runID int64) (RunTaskSummary, error) {
	return LoadRunTaskSummary(ctx, r.q, runID)
}

// maxCopyRows 是单批写入的行数上限（原 spec design「写入策略」、Requirement 28.3）：
// observation 与 occupancy_detail 用 COPY 分批写入，单批不超过 5 万行，批次之间
// 仍在同一个事务里，任何一批失败都会让整个版本整体回滚。
const maxCopyRows = 50000

// ReleaseRoom 是版本归属快照的一行，对应 release_room_building。
// 楼展示名来自本轮白名单，不由 room.building_name 维护（见 store/room.go）。
type ReleaseRoom struct {
	RoomID       string // 房号 jsbh
	BuildingID   string // 归属教学楼编号
	BuildingName string // 归属教学楼展示名
}

// ReleaseObservation 是逐格观测的一行，对应 observation：
// 一间房 × 一周 × 一天 × 一节恰一行（release_id 与学期由草稿补）。
type ReleaseObservation struct {
	RoomID    string // 房号 jsbh
	Week      int    // 教学周
	Weekday   int    // 星期，1=周一 … 7=周日
	Node      string // 小节编号，如 01
	StateKey  string // 语义键
	Available bool   // 按字典投影的可用性
	RawText   string // 状态原文；空串写 NULL
}

// ReleaseDetail 是占用说明的一行，对应 occupancy_detail。
type ReleaseDetail struct {
	RoomID    string // 房号 jsbh
	Weekday   int    // 星期，1=周一 … 7=周日
	Block     string // 大节编码
	StateKey  string // 语义键
	Course    string // 课程或考试科目原文；空串写 NULL
	WeekRange string // 周次范围原文；空串写 NULL
	TimeFlag  string // 时间标志原文；空串写 NULL
	Derived   bool   // 是否用这条说明补写过派生观测
}

// 发布成功时写进 collect_run 的终态与阶段。
//
// 取值与 0001 基线 collect_run.status / phase 的 CHECK 约束一致，也与 pipeline
// 的 RunStatus / Phase 常量同源（publish 不能 import pipeline，依赖方向见
// backend/.golangci.yml；这里只镜像取值，不新造状态）：
//   - 必需任务在发布前已由 Validate 与事务内复核要求全部完成；可选任务全部完成
//     且没有被丢弃的可选明细 → success，有可选失败或被丢弃的可选明细 → partial。
//     failed 不从这里写：失败轮次不发布，由收口写。
//   - 终态与 error_message 在同一条 UPDATE 里写：partial 带原因（最早失败任务的
//     原因，或「丢弃 N 条明细」），success 传空串折成 NULL 清空。
//   - phase 写 publish：发布是最后一个业务阶段。终态与阶段在同一条 UPDATE 里写，
//     轮次不再停留在 running，collect_run_single_running 索引随即让给下一轮。
//     因此终态的 phase 不一定是 finished——判断「已结束」只看 status。
const (
	RunStatusSuccess = "success" // 轮次成功：必需与可选任务全部完成；error_message 清空
	RunStatusPartial = "partial" // 轮次部分完成：可选任务有失败或被丢弃，但可用性数据完整、已发布；error_message 写最早失败原因或丢弃说明
	RunPhasePublish  = "publish" // 发布阶段：与 pipeline.PhasePublish 同值
)

// ReleaseDraft 是一次发布要写进新版本的全部内容，由 publish.Commit 组装。
//
// 它不带「是否 current」：新版本一律先以 is_current=false 写入，切换由
// CommitRelease 在同一个事务的后半段完成。RunID 用于把版本号回写到轮次，
// UpdatedAt 是发布时刻（写 collect_run.updated_at / released_at / finished_at），
// 与 GeneratedAt（版本生成时间，写 release.generated_at）是两个语义。
// WindowStart/WindowEnd 不落库，只作为写层交叉校验的输入：WeeksTotal 必须等于
// 窗口周数、RoomsTotal 必须等于归属快照行数，挡住「汇总计数与实际写进去的行对不上」
// 的坏草稿（管理端会拿这两个计数画进度条）。
// Status 与 ErrorMessage 是发布终态：随回写轮次一起写 collect_run.status 与
// error_message，partial 带原因、success 传空串清空。
type ReleaseDraft struct {
	// ReleaseID 是新版本标识，release.release_id（文本主键）。
	// 格式固定为 `<UTC 秒级>Z`（如 20261023T071012Z），由 publish.releaseIdentifier
	// 唯一产出并在 publish 侧测试里锁定；Store 把它当不透明主键，只校验非空，
	// 不在这里再解析一遍格式——同一条格式规则维护两份只会让两边慢慢漂移。
	// 传入空串或已被占用的标识都会被前置检查拒绝。
	ReleaseID       string    // 版本标识（见上方格式说明）
	RunID           int64     // 轮次编号，成功后回写 collect_run.release_id
	Term            string    // 学期，写 release.term 与 observation.term
	GeneratedAt     time.Time // 版本生成时间，写 release.generated_at
	UpdatedAt       time.Time // 发布时刻，写 collect_run.updated_at 与 released_at
	DictVersion     int       // 字典版本
	AxisVersion     int       // 轴版本
	WindowStart     int       // 采集窗口起始周；只用于写层交叉校验，不落库
	WindowEnd       int       // 采集窗口结束周；只用于写层交叉校验，不落库
	HasAttrConflict bool      // 本轮是否有归属冲突
	UnknownCells    int       // 未知状态格数，回写 collect_run.unknown_cells
	CompositeCells  int       // 复合状态格数，回写 collect_run.composite_cells
	RoomsTotal      int       // 归属快照房号数，回写 collect_run.rooms_total
	WeeksTotal      int       // 采集窗口周数，回写 collect_run.weeks_total
	Status          string    // 发布终态：RunStatusSuccess 或 RunStatusPartial
	ErrorMessage    string    // 发布终态的原因：写 collect_run.error_message，空串折成 NULL（success 由此清空）

	Rooms        []ReleaseRoom        // 归属快照
	Observations []ReleaseObservation // 逐格观测
	Details      []ReleaseDetail      // 占用说明
}

// releaseTx 是一次发布事务需要的最小能力；*pgx.Tx 满足它。
// 写侧只用到 Exec、CopyFrom 与 QueryRow：批量数据走 COPY，版本与轮次的状态用
// Exec，QueryRow 用于发布前置检查（锁轮次）、必需任务完成度复核与回写 0 行后的
// 原因补读。
type releaseTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseTxBeginner 是开启发布事务需要的最小能力，*pgxpool.Pool 满足它。
// 返回 pgx.Tx 而不是 releaseTx：接口方法的返回类型必须与实际仓储一致，
// 事务体再按 releaseTx 消费（pgx.Tx 天然满足，不必包一层适配）。
type releaseTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// insertReleaseSQL 先以非 current 写入新版本。
//
// 有意不在这里切换 current：新版本的观测、说明与归属快照要先全部写完，
// 才允许对外可见；中途任一步失败时旧 current 一直没被降级（CP7）。
const insertReleaseSQL = `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $2, $3, $4, $5, $6, false)`

// demoteCurrentSQL 降原 current。表上的 release_one_current 部分唯一索引保证
// 同时至多一行 current，这里不需要再写 id 条件。
const demoteCurrentSQL = `
UPDATE release
   SET is_current = false
 WHERE is_current`

// promoteCurrentSQL 抬新 current。行数必须为 1：0 行说明新版本没写进去，
// 整个事务必须回滚，否则会留下「没有 current」的库（CP7 的另一半）。
const promoteCurrentSQL = `
UPDATE release
   SET is_current = true
 WHERE release_id = $1`

// deleteOlderReleasesSQL 删更早版本，只保留新 current 与上一版（CP8）。
//
// 「上一版」= 生成时间次新的版本：按 generated_at 倒序取非当前版本里最新的一条，
// release_id 只作同一生成时刻的稳定次序。这里选的是「生成时间」而不是「发布顺序」：
// 补发一批 generated_at 较早的数据时，上一版可能不是上一次实际发布的那一版。
// 要按发布先后保留上一版就得另存发布序列，当前没有这一列，口径固定为生成时间次新
// （用户确认，见 design.md 的 CP8 说明）。
//
// 零个其它版本时 COALESCE 成空串，不会因 NOT IN NULL 把所有版本都删掉。
// release 的观测、说明与归属快照由 ON DELETE CASCADE 一并清掉，
// 指向被删版本的 collect_run.release_id 由 ON DELETE SET NULL 置空；
// 但 collect_run.released_at 不会被外键动作改写，仍是「这一轮发布过」的独立标记。
const deleteOlderReleasesSQL = `
DELETE FROM release
 WHERE release_id <> $1
   AND release_id <> COALESCE((
         SELECT release_id
           FROM release
          WHERE release_id <> $1
          ORDER BY generated_at DESC, release_id DESC
          LIMIT 1), '')`

// preflightRunReleaseSQL 在写入任何行之前锁住轮次并读出发布前置状态：
// 当前关联的版本、独立发布标记、轮次状态，以及目标版本标识是否已被占用。
//
// FOR UPDATE 让同一轮的并发发布串行化：后到者看到先到者写下的 released_at，
// 在写 release 之前就被拒绝，而不是等回写轮次 0 行再回滚整个事务。
// 版本标识检查放在同一条查询里：同 ID 的第二次发布本来会撞 release_pkey
// （只有 SQL 细节的库错误），提前读出来才能返回「该版本标识已存在」。
//
// 轮次不存在时返回 pgx.ErrNoRows，由调用方补成可读错误。
const preflightRunReleaseSQL = `
SELECT COALESCE(release_id, ''),
       released_at,
       status,
       EXISTS (SELECT 1 FROM release WHERE release_id = $2)
  FROM collect_run
 WHERE run_id = $1
 FOR UPDATE`

// writeReleaseSummarySQL 回写轮次汇总、版本号、发布标记与终态（Requirement 23.1）。
//
// 守卫有两道：
//   - released_at IS NULL：一轮只能发布一次。独立标记不随 release 的
//     ON DELETE SET NULL 重置，删掉旧版本后这一轮仍然不能再发；
//   - status = 'running'：收口之后不再发布，也不会把已写下的终态盖掉。
//
// 一条 UPDATE 同时写计数、版本号、发布时刻、终态、阶段、失败原因与 finished_at：
// 切换完成后轮次必然指向新版本且已离开 running，不会出现「版本已经是 current 但
// 轮次还在跑」的中间态。phase 写 publish（不是 finished），终态由 status 表达；
// 轮次不再 running 后 collect_run_single_running 索引立即让给下一轮。
//
// error_message 与收口同口径：partial 写最早失败任务的原因（或丢弃说明），
// success 传空串由 NULLIF 折成 NULL 清空。error_code 不在这里写——结构失败码在
// 任务失败当时就由 pipeline.RecordRunErrorCode 记下了。
const writeReleaseSummarySQL = `
UPDATE collect_run
   SET unknown_cells = $2,
       composite_cells = $3,
       rooms_total = $4,
       weeks_total = $5,
       release_id = $6,
       released_at = $7,
       status = $8,
       phase = $9,
       error_message = NULLIF($10, ''),
       finished_at = $7,
       updated_at = $7
 WHERE run_id = $1
   AND status = 'running'
   AND released_at IS NULL`

// undoneRequiredTasksSQL 在事务里按 sync_task 复核必需任务完成度：只要还有一条
// 必需任务是 pending 或 failed，就不允许把这一轮写成发布终态。
//
// 为什么在事务里再查一遍：publish.Commit 判终态用的是组装候选集时的汇总快照，
// 快照可能是旧的（任务在组装之后才失败、或续跑留下了 failed 行），而发布写的是
// 不可回退的终态。汇总只在 publish 侧判，写侧就仍可能把「必需任务没完成」记成
// success；这一条查询与回写轮次在同一个事务里，轮次行已被 preflight 的
// FOR UPDATE 锁住，读到的至少是这条语句执行时的已提交现状，不会比候选集快照更旧。
//
// 只数非 done 的必需任务，不要求「至少一条必需任务」：空计划轮次由 Validate 与
// 收口拦，Store 只负责「不能把没做完的写成成功」。
const undoneRequiredTasksSQL = `
SELECT count(*)
  FROM sync_task
 WHERE run_id = $1
   AND required
   AND status <> 'done'`

// checkRequiredTasksDone 在发布事务里复核必需任务是否全部完成：还有未完成的
// 就返回可读错误，整个事务回滚。
// status <> 'done' 同时覆盖 pending（执行循环没跑完）与 failed（任务已失败）；
// 两种都不允许写 success/partial——终态描述的是「这一轮的任务都结束了」。
func checkRequiredTasksDone(ctx context.Context, tx releaseTx, runID int64) error {
	var undone int
	if err := tx.QueryRow(ctx, undoneRequiredTasksSQL, runID).Scan(&undone); err != nil {
		return fmt.Errorf("%w: 复核轮次 %d 的必需任务完成度失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	if undone > 0 {
		return fmt.Errorf("%w: 轮次 %d 还有 %d 条必需任务未完成（pending/failed），不能写发布终态",
			model.ErrRunStoreFailed, runID, undone)
	}
	return nil
}

// selectRunReleaseSQL 在 0 行回写后区分原因：轮次不存在、已发布过版本（独立标记
// 或旧 release_id 仍在）、已收口（status 不再是 running）。
const selectRunReleaseSQL = `
SELECT COALESCE(release_id, ''), released_at, status
  FROM collect_run
 WHERE run_id = $1`

// validateReleaseDraft 校验草稿的身份字段与数据非空。
//
// 业务门槛（必需任务是否完成、归属是否完整）属于 publish.Validate，不在这里重复；
// 这里只挡「写进去会破坏库或让线上变空」的硬错误：版本号、轮次号、学期、
// 版本生成时间、发布时刻都必须有，未知/复合不能是负数，房号数与窗口周数必须为正
// 且与归属快照行数、采集窗口跨度一致，终态只能是 success/partial，观测与归属快照
// 不能为空（空版本会让公开查询整体变成空态，Requirement 7.1）。
func validateReleaseDraft(draft ReleaseDraft) error {
	if draft.ReleaseID == "" {
		return fmt.Errorf("%w: 发布缺少版本标识", model.ErrRunStoreFailed)
	}
	if draft.RunID <= 0 {
		return fmt.Errorf("%w: 发布缺少轮次编号: %d", model.ErrRunStoreFailed, draft.RunID)
	}
	if draft.Term == "" {
		return fmt.Errorf("%w: 轮次 %d 的发布缺少学期", model.ErrRunStoreFailed, draft.RunID)
	}
	if draft.GeneratedAt.IsZero() {
		return fmt.Errorf("%w: 轮次 %d 的发布缺少版本生成时间", model.ErrRunStoreFailed, draft.RunID)
	}
	if draft.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: 轮次 %d 的发布缺少发布时刻", model.ErrRunStoreFailed, draft.RunID)
	}
	if draft.DictVersion <= 0 || draft.AxisVersion <= 0 {
		return fmt.Errorf("%w: 轮次 %d 的字典版本 %d / 轴版本 %d 非法",
			model.ErrRunStoreFailed, draft.RunID, draft.DictVersion, draft.AxisVersion)
	}
	if draft.UnknownCells < 0 || draft.CompositeCells < 0 {
		return fmt.Errorf("%w: 轮次 %d 的未知/复合计数不能为负: %d / %d",
			model.ErrRunStoreFailed, draft.RunID, draft.UnknownCells, draft.CompositeCells)
	}
	// 两个计数要回写 collect_run 并向管理端展示。0 说明候选集没算计数，
	// 写进去会让轮次显示成「零间房、零周」，比拒绝发布更难排查。
	if draft.RoomsTotal <= 0 || draft.WeeksTotal <= 0 {
		return fmt.Errorf("%w: 轮次 %d 的房号数 %d / 窗口周数 %d 非法",
			model.ErrRunStoreFailed, draft.RunID, draft.RoomsTotal, draft.WeeksTotal)
	}
	// 写层交叉校验：房号数必须等于真正会写进 release_room_building 的行数，
	// 窗口周数必须等于窗口跨度。只校验正数挡不住「计数 3、快照 1 行」这种坏草稿，
	// 写进去后管理端显示的进度与实际版本对不上，而且没有任何报错。
	if draft.WindowStart < 1 || draft.WindowEnd < draft.WindowStart {
		return fmt.Errorf("%w: 轮次 %d 的采集窗口非法: %d..%d",
			model.ErrRunStoreFailed, draft.RunID, draft.WindowStart, draft.WindowEnd)
	}
	if want := draft.WindowEnd - draft.WindowStart + 1; draft.WeeksTotal != want {
		return fmt.Errorf("%w: 轮次 %d 的窗口周数 %d 与采集窗口 %d..%d 不一致",
			model.ErrRunStoreFailed, draft.RunID, draft.WeeksTotal, draft.WindowStart, draft.WindowEnd)
	}
	if draft.RoomsTotal != len(draft.Rooms) {
		return fmt.Errorf("%w: 轮次 %d 的房号数 %d 与归属快照 %d 行不一致",
			model.ErrRunStoreFailed, draft.RunID, draft.RoomsTotal, len(draft.Rooms))
	}
	// 发布写的是终态：只接受 success 与 partial。failed / running 不从这里写，
	// 避免一条坏草稿把线上轮次改成「发布失败」却留下新版本。
	if draft.Status != RunStatusSuccess && draft.Status != RunStatusPartial {
		return fmt.Errorf("%w: 轮次 %d 的发布终态 %q 非法（只能是 %s 或 %s）",
			model.ErrRunStoreFailed, draft.RunID, draft.Status, RunStatusSuccess, RunStatusPartial)
	}
	if len(draft.Observations) == 0 {
		return fmt.Errorf("%w: 轮次 %d 的版本没有观测", model.ErrRunStoreFailed, draft.RunID)
	}
	// 没有归属快照的版本写进去后，公开查询按归属过滤会整体查不到教室——
	// 这同样是「线上变空」，在开事务前挡住。逐行覆盖检查属于 publish.Validate。
	if len(draft.Rooms) == 0 {
		return fmt.Errorf("%w: 轮次 %d 的版本没有归属快照", model.ErrRunStoreFailed, draft.RunID)
	}
	return nil
}

// runReleasePreflight 是 preflightRunReleaseSQL 读回的发布前置状态。
type runReleasePreflight struct {
	ReleaseID     string     // 当前关联的版本；NULL 折成空串
	ReleasedAt    *time.Time // 独立发布标记；nil 表示从未发布
	Status        string     // collect_run.status
	ReleaseExists bool       // 目标版本标识是否已被某个 release 占用
}

// checkRunReleasable 在写入任何行之前检查这一轮还能不能发布。
//
// 这是「一轮只能发布一次」的前置守卫：released_at 是独立标记，不随 release 的
// ON DELETE SET NULL 重置；旧库上已经关联过 release_id 的轮次同样拒绝。检查在
// 同一个事务里用 FOR UPDATE 锁住轮次，后到的并发发布会在写 release 之前失败，
// 而不是写完版本才在回写轮次时撞守卫再回滚。同 ID 撞 release_pkey 也在这里
// 提前拦下，返回可读原因而不是一条只有主键名的库错误。
func checkRunReleasable(ctx context.Context, tx releaseTx, draft ReleaseDraft) error {
	var pre runReleasePreflight
	err := tx.QueryRow(ctx, preflightRunReleaseSQL, draft.RunID, draft.ReleaseID).Scan(
		&pre.ReleaseID, &pre.ReleasedAt, &pre.Status, &pre.ReleaseExists)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法发布", model.ErrRunStoreFailed, draft.RunID)
	}
	if err != nil {
		return fmt.Errorf("%w: 读取轮次 %d 的发布状态失败: %w", model.ErrRunStoreFailed, draft.RunID, err)
	}
	if pre.ReleasedAt != nil {
		// released_at 在、release_id 空：CP8 删旧版本时外键把关联置空了，
		// 版本本身已经不在库里。文案要说明「版本已被清理」，不能让运维拿一个
		// 空版本号去查 release。
		if pre.ReleaseID == "" {
			return fmt.Errorf("%w: 轮次 %d 已发布过（版本已被清理，released_at=%s），不能重复发布",
				model.ErrRunStoreFailed, draft.RunID, pre.ReleasedAt.In(time.UTC).Format(time.RFC3339))
		}
		return fmt.Errorf("%w: 轮次 %d 已发布过版本 %s（发布于 %s），不能重复发布",
			model.ErrRunStoreFailed, draft.RunID, pre.ReleaseID,
			pre.ReleasedAt.In(time.UTC).Format(time.RFC3339))
	}
	if pre.ReleaseID != "" {
		return fmt.Errorf("%w: 轮次 %d 已发布过版本 %s，不能重复发布",
			model.ErrRunStoreFailed, draft.RunID, pre.ReleaseID)
	}
	if pre.Status != "running" {
		return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能发布",
			model.ErrRunStoreFailed, draft.RunID, pre.Status)
	}
	if pre.ReleaseExists {
		return fmt.Errorf("%w: 版本标识 %s 已存在（可能是同一生成秒的其他轮次），不能重复使用",
			model.ErrRunStoreFailed, draft.ReleaseID)
	}
	return nil
}

// CommitRelease 在单个事务里写入新版本并切换 current（design 的 publish.Commit、
// Requirement 7.1–7.4、CP7/CP8）。
//
// 步骤顺序固定，且在同一个事务里：
//  1. 锁住轮次并做发布前置检查（未发布过、仍在 running、版本标识未占用）；
//  2. 按 sync_task 复核必需任务完成度（还有 pending/failed 就整体拒绝）；
//  3. 写 release（is_current=false）；
//  4. COPY 观测（分批 ≤ maxCopyRows）；
//  5. COPY 占用说明（同样分批）；
//  6. COPY 归属快照；
//  7. 降原 current；
//  8. 抬新 current（必须命中 1 行）；
//  9. 删更早版本，只留新 current 与上一版；
//  10. 回写轮次汇总、release_id 与 released_at，并写终态 success/partial、
//     phase=publish、error_message、finished_at（轮次不再 running）。
//
// 任一步失败都整体回滚：旧 current 从未被降级或已恢复原状，库里不会出现
// 0 个或 2 个 current；也不会有半批观测被留下（CP7）。成功后新版本才对外可见，
// 轮次同时离开 running——调用方不应再对这一轮调用 FinishRun/CloseRun。
func CommitRelease(ctx context.Context, pool releaseTxBeginner, draft ReleaseDraft) error {
	if err := validateReleaseDraft(draft); err != nil {
		return err
	}
	if pool == nil {
		return fmt.Errorf("%w: 发布缺少数据库连接", model.ErrRunStoreFailed)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: 发布事务开启失败: %w", model.ErrRunStoreFailed, err)
	}
	// 提交成功后再回滚是空操作；所有失败路径都靠这里收尾（与 migrate.go 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := writeRelease(ctx, tx, draft); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: 发布事务提交失败: %w", model.ErrRunStoreFailed, err)
	}
	return nil
}

// writeRelease 是 CommitRelease 的事务体：只负责按固定顺序写，不负责开事务。
// 拆出来是为了让「顺序、批大小、失败即停」能用一个记录型事务替身单独验证，
// 不必连库（真实库口径由集成测试覆盖）。
func writeRelease(ctx context.Context, tx releaseTx, draft ReleaseDraft) error {
	if err := checkRunReleasable(ctx, tx, draft); err != nil {
		return err
	}
	// 轮次行已被前置检查锁住，这里读到的是本事务内的现状：还有 pending/failed
	// 的必需任务就整体拒绝，不写 release、也不写终态。
	if err := checkRequiredTasksDone(ctx, tx, draft.RunID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertReleaseSQL,
		draft.ReleaseID, draft.Term, draft.GeneratedAt,
		draft.DictVersion, draft.AxisVersion, draft.HasAttrConflict); err != nil {
		return fmt.Errorf("%w: 写入版本 %s 失败: %w", model.ErrRunStoreFailed, draft.ReleaseID, err)
	}
	if err := copyInBatches(ctx, tx, pgx.Identifier{"observation"}, observationColumns,
		len(draft.Observations), func(index int) ([]any, error) {
			row := draft.Observations[index]
			return []any{
				draft.ReleaseID, row.RoomID, draft.Term,
				int16(row.Week), int16(row.Weekday), row.Node,
				row.StateKey, row.Available, nullableText(row.RawText),
			}, nil
		}); err != nil {
		return fmt.Errorf("%w: 写入版本 %s 的观测失败: %w", model.ErrRunStoreFailed, draft.ReleaseID, err)
	}
	if err := copyInBatches(ctx, tx, pgx.Identifier{"occupancy_detail"}, detailColumns,
		len(draft.Details), func(index int) ([]any, error) {
			row := draft.Details[index]
			return []any{
				draft.ReleaseID, row.RoomID, int16(row.Weekday), row.Block, row.StateKey,
				nullableText(row.Course), nullableText(row.WeekRange), nullableText(row.TimeFlag),
				row.Derived,
			}, nil
		}); err != nil {
		return fmt.Errorf("%w: 写入版本 %s 的占用说明失败: %w", model.ErrRunStoreFailed, draft.ReleaseID, err)
	}
	if err := copyInBatches(ctx, tx, pgx.Identifier{"release_room_building"}, roomBuildingColumns,
		len(draft.Rooms), func(index int) ([]any, error) {
			row := draft.Rooms[index]
			return []any{draft.ReleaseID, row.RoomID, row.BuildingID, row.BuildingName}, nil
		}); err != nil {
		return fmt.Errorf("%w: 写入版本 %s 的归属快照失败: %w", model.ErrRunStoreFailed, draft.ReleaseID, err)
	}

	if _, err := tx.Exec(ctx, demoteCurrentSQL); err != nil {
		return fmt.Errorf("%w: 降原 current 失败: %w", model.ErrRunStoreFailed, err)
	}
	tag, err := tx.Exec(ctx, promoteCurrentSQL, draft.ReleaseID)
	if err != nil {
		return fmt.Errorf("%w: 抬新 current 失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: 版本 %s 抬为 current 时命中 %d 行，发布中止",
			model.ErrRunStoreFailed, draft.ReleaseID, tag.RowsAffected())
	}
	if _, err := tx.Exec(ctx, deleteOlderReleasesSQL, draft.ReleaseID); err != nil {
		return fmt.Errorf("%w: 删除更早版本失败: %w", model.ErrRunStoreFailed, err)
	}

	// success 轮次不保留失败原因：即便调用方误传了文案，也在这里清空，不把
	// 上一状态的失败原因留在成功轮次上（partial 才带原因，见 writeReleaseSummarySQL）。
	if draft.Status == RunStatusSuccess {
		draft.ErrorMessage = ""
	}

	tag, err = tx.Exec(ctx, writeReleaseSummarySQL,
		draft.RunID, draft.UnknownCells, draft.CompositeCells,
		draft.RoomsTotal, draft.WeeksTotal,
		draft.ReleaseID, draft.UpdatedAt, draft.Status, RunPhasePublish,
		draft.ErrorMessage)
	if err != nil {
		return fmt.Errorf("%w: 回写轮次 %d 的发布汇总失败: %w", model.ErrRunStoreFailed, draft.RunID, err)
	}
	if tag.RowsAffected() == 0 {
		return runReleaseWritebackError(ctx, tx, draft)
	}
	return nil
}

// observationColumns、detailColumns、roomBuildingColumns 是 COPY 的列顺序，
// 与 schema 一一对应；改列时必须同步改这里，否则 COPY 会写入错列。
var (
	observationColumns  = []string{"release_id", "room_id", "term", "week", "weekday", "node", "state_key", "available", "raw_text"}
	detailColumns       = []string{"release_id", "room_id", "weekday", "block", "state_key", "course", "week_range", "time_flag", "derived"}
	roomBuildingColumns = []string{"release_id", "room_id", "building_id", "building_name"}
)

// copyInBatches 把 total 行按 maxCopyRows 切批写进 table。
//
// 每批一次 CopyFrom：批内整批成功或整批失败，批间仍在调用方的事务里，
// 任意一批失败都会让调用方整体回滚。row 按「全局行号」取行，切批不会错位。
// 0 行时不发 COPY：PostgreSQL 允许空 COPY，但多一次往返没有意义，
// 也便于测试断言「没有行就没有语句」。
func copyInBatches(
	ctx context.Context,
	tx releaseTx,
	table pgx.Identifier,
	columns []string,
	total int,
	row func(index int) ([]any, error),
) error {
	for start := 0; start < total; start += maxCopyRows {
		end := start + maxCopyRows
		if end > total {
			end = total
		}
		if _, err := tx.CopyFrom(ctx, table, columns,
			pgx.CopyFromSlice(end-start, func(offset int) ([]any, error) {
				return row(start + offset)
			})); err != nil {
			return err
		}
	}
	return nil
}

// runReleaseWritebackError 在轮次汇总 0 行更新后区分原因：
// 轮次不存在、已发布过版本（独立标记或旧 release_id）、轮次已收口。
// 正常情况下前置检查已经挡住这些情况，这里只兜并发或绕过前置检查的写入。
func runReleaseWritebackError(ctx context.Context, tx releaseTx, draft ReleaseDraft) error {
	var (
		existing   string
		releasedAt *time.Time
		status     string
	)
	err := tx.QueryRow(ctx, selectRunReleaseSQL, draft.RunID).Scan(&existing, &releasedAt, &status)
	// 读不到这一轮：SQL 已经 0 行，补读也没有行，说明轮次确实不存在。
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法回写发布汇总", model.ErrRunStoreFailed, draft.RunID)
	}
	if err != nil {
		return fmt.Errorf("%w: 轮次 %d 回写发布汇总时未命中，且无法确认原因: %w",
			model.ErrRunStoreFailed, draft.RunID, err)
	}
	if releasedAt != nil {
		if existing == "" {
			return fmt.Errorf("%w: 轮次 %d 已发布过（版本已被清理，released_at=%s），不能重复发布",
				model.ErrRunStoreFailed, draft.RunID,
				releasedAt.In(time.UTC).Format(time.RFC3339))
		}
		return fmt.Errorf("%w: 轮次 %d 已发布过版本 %s（发布于 %s），不能重复发布",
			model.ErrRunStoreFailed, draft.RunID, existing,
			releasedAt.In(time.UTC).Format(time.RFC3339))
	}
	if existing != "" {
		return fmt.Errorf("%w: 轮次 %d 已发布过版本 %s，不能重复发布",
			model.ErrRunStoreFailed, draft.RunID, existing)
	}
	if status != "running" {
		return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能发布",
			model.ErrRunStoreFailed, draft.RunID, status)
	}
	return fmt.Errorf("%w: 轮次 %d 不存在，无法回写发布汇总", model.ErrRunStoreFailed, draft.RunID)
}

// nullableText 把空串折成 NULL，让可空列不留下空字符串（与轮次收口的
// NULLIF 口径一致）；非空原样返回。
func nullableText(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// ReleaseRepo 是发布写侧仓储：把连接池闭在内部，向 publish 暴露一个
// 「提交整个版本」的方法。publish 不接触 pgx、SQL 与表结构。
type ReleaseRepo struct {
	pool releaseTxBeginner
}

// NewReleaseRepo 绑定一个连接池，通常是 *pgxpool.Pool。
func NewReleaseRepo(pool releaseTxBeginner) ReleaseRepo {
	return ReleaseRepo{pool: pool}
}

// CommitRelease 见同名函数。
func (r ReleaseRepo) CommitRelease(ctx context.Context, draft ReleaseDraft) error {
	return CommitRelease(ctx, r.pool, draft)
}
