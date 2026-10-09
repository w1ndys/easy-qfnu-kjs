// 本文件是 entry 层：任务执行器。周矩阵任务请求、清洗、写房间目录并进本轮候选集；
// 格子明细任务请求、清洗并暂存说明。请求与清洗的细则在 fetch 与 clean 两层，
// 本文件只负责把两类任务接上这两层（需求 3.1、3.2、4.1、4.2）。

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/sync"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// errUnknownTaskKind 表示取到了不认识的续传单元：库里只有周矩阵与明细两类。
var errUnknownTaskKind = errors.New("不认识的同步任务种类")

// roundWorker 是入口串好的任务执行器：一轮一个，候选集暂存在它持有的 round 上。
type roundWorker struct {
	collector *Collector // 编排者，提供客户端与房间目录写入
	round     *round     // 本轮的候选集与目录
}

// RunTask 执行一个任务，返回本次发出的上游请求次数。
// 会话失效原样返回给状态机，由它重新登录后再跑同一个任务（需求 5.3）。
func (w *roundWorker) RunTask(ctx context.Context, task store.SyncTask) (int, error) {
	// 两类任务的列形状不同：周矩阵带周次，明细带房间、星期与大节
	if task.Kind == store.KindWeekMatrix {
		return w.runWeekMatrix(ctx, task)
	}
	if task.Kind == store.KindCellDetail {
		return w.runCellDetail(ctx, task)
	}
	// 库里不会出现第三种种类，出现说明取到了定位不了格子的续传单元
	return 0, fmt.Errorf("%w: %s", errUnknownTaskKind, task.Kind)
}

// runWeekMatrix 请求一栋楼某一周的周矩阵，清洗后写房间目录并进本轮候选集（需求 2.4、3.1、3.2）。
func (w *roundWorker) runWeekMatrix(ctx context.Context, task store.SyncTask) (int, error) {
	// 白名单里的展示名要随房间一起入库，任务只带编号，所以先按编号取那一条
	building, err := w.round.buildingOf(task.BuildingID)
	if err != nil {
		return 0, err
	}

	response, err := w.collector.client.WeekMatrix(ctx, fetch.WeekMatrixParams{
		Term:       w.round.term,
		BuildingID: task.BuildingID,
		Week:       task.Week,
	})
	if err != nil {
		return requestAttempts(err), err
	}

	matrix, err := clean.Parse(response.Body, w.round.axis, w.round.symbols)
	// 表头大节或结构变了：这一轮的解析前提没了，整轮停止，后面不开始
	if err != nil {
		return response.Attempts, sync.StopRun(err)
	}
	if err := w.collector.upsertRooms(ctx, w.round, building, matrix); err != nil {
		return response.Attempts, err
	}
	w.round.weeks = append(w.round.weeks, publish.MatrixWeek{Week: task.Week, Matrix: matrix})
	return response.Attempts, nil
}

// runCellDetail 请求一个格子的占用明细，清洗后暂存到本轮候选集（需求 4.1、4.2）。
func (w *roundWorker) runCellDetail(ctx context.Context, task store.SyncTask) (int, error) {
	response, err := w.collector.client.CellDetail(ctx, fetch.CellDetailParams{
		Term:    w.round.term,
		RoomID:  task.RoomID,
		Weekday: task.Weekday,
		Block:   task.Block,
	})
	if err != nil {
		return requestAttempts(err), err
	}

	details, err := clean.ParseCellDetail(response.Body, w.round.states)
	// 这一格的弹窗结构读不出来只影响它自己：其余格子继续跑；本轮因为有失败任务不会发布
	if err != nil {
		return response.Attempts, err
	}
	w.round.details = append(w.round.details, publish.CellDetail{
		RoomID:  task.RoomID,
		Weekday: task.Weekday,
		Block:   task.Block,
		Details: details,
	})
	return response.Attempts, nil
}

// requestAttempts 取出一次失败里已经发出的请求次数：非请求类失败按 0 记。
func requestAttempts(err error) int {
	var requestErr fetch.RequestError
	// 只有请求类失败带请求次数，其余失败根本没有试过上游
	if errors.As(err, &requestErr) {
		return requestErr.Attempts
	}
	return 0
}
