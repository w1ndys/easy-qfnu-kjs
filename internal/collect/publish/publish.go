// 本文件属于 business 层：一轮全量跑完后的发布层——发布前校验 + 单事务换 current。
// 只有全部必需任务完成且校验通过才发布；失败、阻断或发布事务失败都保留原 current。
// 依据 specs/collector-full-sync/requirements.md 的 2.5、6.1、6.2 与设计文档 Error Handling；
// 校验项与阈值见 docs/contract/data-format.v2.md 的 validate 与 publish 两层。
// 本层不发 HTTP、不解析正文：候选集由入口串好之后传进来。

package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// releaseIDLayout 是版本 id 的可读格式：时间戳串，与库里已有版本 id 的写法一致。
const releaseIDLayout = "20060102150405.000000"

// ErrTasksIncomplete 表示本轮还有任务没完成：发布要等全部必需任务完成（需求 6.1）。
var ErrTasksIncomplete = errors.New("本轮任务未全部完成，拒绝发布")

// ReleaseStore 是本层需要的存储能力，实现在 internal/store；测试用内存实现替换。
type ReleaseStore interface {
	// SyncProgress 读本轮任务的完成数与总数，用来判断必需任务是否都完成了
	SyncProgress(ctx context.Context, runID int64) (int, int, error)
	// PublishRelease 在一个事务里写出新版本并切换 current；失败时 current 保持原样
	PublishRelease(ctx context.Context, batch store.PublishBatch) error
}

// Candidate 是本轮候选集：要发布的观测与明细，加上校验与展开需要的房间目录和节次轴。
type Candidate struct {
	Term        string           // 学期编号
	DictVersion int              // 本轮用的字典版本
	AxisVersion int              // 本轮用的节次轴版本
	TotalWeeks  int              // 学期总周数，展开明细的周次范围要用
	Axis        []model.AxisNode // 当前节次轴：大节与它的小节，按展示顺序传入
	Rooms       []model.Room     // 本轮房间目录，每间带返回它的那次请求的楼
	Weeks       []MatrixWeek     // 各栋楼各周的周矩阵
	Details     []CellDetail     // 下钻到的格子与它的占用说明
	StopErr     error            // 上游整轮停止类错误（表头块集合变化等）：非空时不得发布
}

// MatrixWeek 是一栋楼某一周的清洗结果：发布时按周展开成该周的观测行。
type MatrixWeek struct {
	Week   int          // 教学周次，从 1 开始
	Matrix clean.Matrix // 该周清洗出的矩阵：房间身份、房名与 7×12 小节状态
}

// CellDetail 是一个格子（房间 × 星期 × 大节）的占用说明，同一格可能有多条。
type CellDetail struct {
	RoomID  string         // 房间身份 jsbh
	Weekday int            // 1=周一 … 7=周日
	Block   string         // 大节编码，取值见 axis_node.block
	Details []clean.Detail // 该格的说明行；弹窗里没有记录时为空
}

// Result 是一次发布的规模与计数，供调用方写入运行记录与告警。
type Result struct {
	ReleaseID      string // 新发布的版本 id
	Rooms          int    // 本版房间总数（去重）
	Observations   int    // 写入的观测行数
	Details        int    // 写入的占用明细行数
	UnknownCells   int    // 未知符号的格子数，只计数不阻断发布
	CompositeCells int    // 复合符号的格子数，只计数不阻断发布
}

// Publisher 是发布层：校验候选集，通过后用一次事务换 current。
type Publisher struct {
	store ReleaseStore     // 轮次进度与事务发布
	now   func() time.Time // 时钟，测试可注入固定时间以固定版本 id
}

// NewPublisher 组装发布层；now 可注入，便于测试固定版本 id。
func NewPublisher(releaseStore ReleaseStore, now func() time.Time) *Publisher {
	// 调用方没给时钟就用系统时间，生产路径走这里
	if now == nil {
		now = time.Now
	}
	return &Publisher{store: releaseStore, now: now}
}

// Publish 发布一轮：确认全部必需任务完成、校验通过，再用一次事务写入并切换 current。
// 任何一步不通过都直接返回错误，原 current 不动（需求 6.2）；Result 只在成功时有意义。
func (p *Publisher) Publish(ctx context.Context, runID int64, candidate Candidate) (Result, error) {
	if err := p.checkTasksDone(ctx, runID); err != nil {
		return Result{}, err
	}

	generatedAt := p.now()
	releaseID := generatedAt.Format(releaseIDLayout)

	built, err := buildPlan(candidate, releaseID)
	if err != nil {
		return Result{}, err
	}

	// 校验不过就地返回：本轮不发布，current 保持原样
	if err := validate(built, candidate); err != nil {
		return Result{}, err
	}

	batch := store.PublishBatch{
		Release: model.Release{
			ID:          releaseID,
			Term:        candidate.Term,
			GeneratedAt: generatedAt,
			DictVersion: candidate.DictVersion,
			AxisVersion: candidate.AxisVersion,
		},
		Observations: built.observations,
		Details:      built.details,
	}
	// 事务失败时 current 保持原样，错误原样交给调用方
	if err := p.store.PublishRelease(ctx, batch); err != nil {
		return Result{}, err
	}

	return Result{
		ReleaseID:      releaseID,
		Rooms:          built.rooms,
		Observations:   len(built.observations),
		Details:        len(built.details),
		UnknownCells:   built.unknownCells,
		CompositeCells: built.compositeCells,
	}, nil
}

// checkTasksDone 确认本轮全部必需任务都完成了：done 少于 total 说明有任务失败或还没跑完。
// 一个任务都没有说明任务清单没建起来，同样不该发布。
func (p *Publisher) checkTasksDone(ctx context.Context, runID int64) error {
	done, total, err := p.store.SyncProgress(ctx, runID)
	if err != nil {
		return err
	}
	// 完成数没追平总数：本轮还有失败或待办的任务，发布必须等它们全部完成
	if total == 0 || done < total {
		return fmt.Errorf("%w: 已完成 %d/%d 个任务", ErrTasksIncomplete, done, total)
	}
	return nil
}
