// 本文件是 entry 层：采集器的全量编排——登录、读学期、采样周历、各周矩阵、聚合矩阵、
// 格子明细、校验与发布按固定顺序执行，前一步被定为整轮失败时后面不开始。
// 请求与清洗分别由 internal/collect/fetch 与 internal/collect/clean 负责，本文件只串流程；
// 手动开始与定时触发都进 Start，跑的是同一套任务（需求 5.4、5.5 与设计文档 Architecture）。

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/calendar"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/sync"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// errNoRun 表示本轮还没有开过轮次：明细任务与收尾都需要轮次 id。
var errNoRun = errors.New("本轮还没有开始，取不到轮次 id")

// RoundStore 是采集器需要的数据能力，实现在 internal/store；测试注入内存实现，不连数据库。
// 它同时覆盖任务状态机（sync.TaskStore）与发布层（publish.ReleaseStore）要的两组能力。
type RoundStore interface {
	sync.TaskStore       // 轮次开关、任务清单、任务状态与进度
	publish.ReleaseStore // 进度统计与单事务发布
	// ReplaceTermWeeks 整体替换某学期的周历，不留半截
	ReplaceTermWeeks(ctx context.Context, term string, weeks []model.TermWeek) error
	// UpsertRoom 写入房间目录；同一 jsbh 换楼时返回 store.ErrRoomBuildingConflict
	UpsertRoom(ctx context.Context, room model.Room) error
	// LatestDictVersion 读当前字典版本，分类用的符号表按它取
	LatestDictVersion(ctx context.Context) (int, error)
	// LatestAxisVersion 读当前节次轴版本，展开小节按它取
	LatestAxisVersion(ctx context.Context) (int, error)
	// DictSymbols 读某个字典版本的符号表
	DictSymbols(ctx context.Context, dictVersion int) ([]model.DictSymbol, error)
	// DictStates 读某个字典版本的状态表，明细解析用它认状态名
	DictStates(ctx context.Context, dictVersion int) ([]model.DictState, error)
	// AxisNodes 读某个轴版本的 12 个小节，按展示顺序
	AxisNodes(ctx context.Context, axisVersion int) ([]model.AxisNode, error)
	// CurrentRelease 读当前已发布版本，用来判断学期是否切换
	CurrentRelease(ctx context.Context) (model.Release, error)
	// LatestRunStatus 读最近一轮运行状态，用来判断连续失败后的首次恢复
	LatestRunStatus(ctx context.Context) (string, bool, error)
	// RecordAlert 落 alert_log 去重，返回该键是否首次出现
	RecordAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error)
}

// Session 是上游会话：既能提供请求要带的 Cookie，也能在失效时重新登录。
// 真实 CAS 与 ddddocr 见 session.go 的未实现入口，测试注入假会话。
type Session interface {
	// Login 登录并刷新会话
	Login(ctx context.Context) error
	// Cookie 返回当前会话的 Cookie 头；没有可用会话时返回空串
	Cookie() string
}

// roundBefore 是本轮开始前的状态：上一份快照的学期与最近一轮的运行状态。
type roundBefore struct {
	term       string // 上一份已发布快照的学期；还没有快照时为空
	hasRelease bool   // 库里是否已有发布版本
	status     string // 最近一轮的运行状态；还没有轮次时为空
	hasRun     bool   // 库里是否已有采集轮次
}

// round 是一轮采集的候选集与目录：采集期间暂存在内存里，发布时一次性交给发布层。
type round struct {
	term        string               // 学期编号，请求的 xnxqh 与观测的 term
	dictVersion int                  // 本轮分类用的字典版本
	axisVersion int                  // 本轮用的节次轴版本
	totalWeeks  int                  // 学期总教学周数
	today       time.Time            // 本轮的当天日期，写进房间目录的首见与最近时间
	axis        []model.AxisNode     // 节次轴，按展示顺序
	symbols     []model.DictSymbol   // 符号表，按版本读出，不写死在代码里
	states      []model.DictState    // 状态表，明细里的中文状态名按它换成语义键
	buildings   []store.Building     // 本轮的白名单教学楼
	weeks       []publish.MatrixWeek // 各楼各周的周矩阵
	details     []publish.CellDetail // 下钻到的格子与它的占用说明
	rooms       []model.Room         // 房间目录，发布前按它查同一 jsbh 是否归属两栋楼
}

// collectorConfig 是装配采集器需要的东西：数据层、上游地址、会话与时钟。
type collectorConfig struct {
	store      RoundStore          // 数据层
	baseURL    string              // 教务系统根地址，部署级变量传入
	session    Session             // 上游会话；为空时用未实现的 CAS 会话
	httpClient *http.Client        // 上游 HTTP 客户端；为空时用默认客户端
	logger     *slog.Logger        // 进程日志
	location   *time.Location      // Asia/Shanghai
	now        func() time.Time    // 时钟，测试注入固定时间
	sleep      func(time.Duration) // 相邻请求间隔的等待函数，测试注入假等待
	notifier   Notifier            // 告警发送方；为空时只写进程日志
}

// Collector 是采集器：手动开始与定时触发都调用 Start。
type Collector struct {
	tracker   *runTracker        // 记下轮次 id 的数据层包装
	client    *fetch.Client      // 上游请求
	session   Session            // 上游会话
	publisher *publish.Publisher // 校验与单事务发布
	alerter   *Alerter           // 告警去重
	logger    *slog.Logger       // 进程日志
	location  *time.Location     // Asia/Shanghai
	now       func() time.Time   // 时钟
}

// runTracker 包住数据层，记下 StartSyncRun 返回的轮次 id。
// 聚合矩阵与明细任务要在同一轮里补齐，而轮次 id 只有开轮次这一步拿得到。
type runTracker struct {
	RoundStore       // 其余数据能力原样转调
	runID      int64 // 本轮轮次 id
	started    bool  // 是否已经开过轮次
}

// StartSyncRun 开一轮并记下轮次 id；被拒绝（已有一轮 running）时不覆盖上一次记下的 id。
func (t *runTracker) StartSyncRun(ctx context.Context, term string) (int64, error) {
	runID, err := t.RoundStore.StartSyncRun(ctx, term)
	// 开成功才更新轮次 id：被拒绝时本轮并没有新轮次
	if err == nil {
		t.runID = runID
		t.started = true
	}
	return runID, err
}

// currentRunID 返回本轮轮次 id；还没开过轮次时返回 errNoRun。
func (t *runTracker) currentRunID() (int64, error) {
	// 没开过轮次说明失败发生在建任务之前，此时没有可收尾的记录
	if !t.started {
		return 0, errNoRun
	}
	return t.runID, nil
}

// newCollector 装配采集器：客户端、任务状态机与发布层都从这里接上。
func newCollector(cfg collectorConfig) (*Collector, error) {
	// 数据层与网络客户端缺任何一个都跑不起来，宁可启动失败也不要一轮都跑不动
	if cfg.store == nil {
		return nil, errors.New("数据层为空，无法装配采集器")
	}

	session := cfg.session
	// 没给会话就用未实现的 CAS 会话：登录会在第一轮明确报未实现，而不是静默拿空 Cookie 去请求
	if session == nil {
		session = newCASSession(cfg.baseURL)
	}
	client, err := fetch.NewClient(fetch.ClientConfig{
		BaseURL:    cfg.baseURL,
		Session:    session,
		HTTPClient: cfg.httpClient,
		Now:        cfg.now,
		Sleep:      cfg.sleep,
	})
	if err != nil {
		return nil, err
	}

	notifier := cfg.notifier
	// 没给发送方时只写进程日志：面板与飞书落地前先保证告警可查
	if notifier == nil {
		notifier = logNotifier{logger: cfg.logger}
	}

	tracker := &runTracker{RoundStore: cfg.store}
	return &Collector{
		tracker:   tracker,
		client:    client,
		session:   session,
		publisher: publish.NewPublisher(tracker, cfg.now),
		alerter:   newAlerter(tracker, notifier, cfg.location),
		logger:    cfg.logger,
		location:  cfg.location,
		now:       cfg.now,
	}, nil
}

// Start 跑一轮全量：手动开始与定时触发共用它，同一时刻至多一轮由存储的 running 判断拦住。
func (c *Collector) Start(ctx context.Context) error {
	// 本轮开始前的状态必须先读：学期切换与首次恢复都按它判断，发布之后就换了
	before, err := c.readBefore(ctx)
	if err != nil {
		return err
	}

	current, err := c.prepare(ctx)
	// 登录、学期或周历任一步失败：这一步之后才开始发教室状态请求，本轮不发布
	if err != nil {
		c.notify(ctx, alarmFinalFailure, "", err.Error())
		return err
	}

	if err := c.collect(ctx, current); err != nil {
		// 整轮停止、任务失败与聚合矩阵失败都在这里：本轮不发布，原 current 不动
		c.notifyFailure(ctx, current.term, err)
		return err
	}

	if err := c.publishRound(ctx, current); err != nil {
		// 发布被拦或事务失败：按校验拦截通知一次，原 current 保持原样
		occasion := alarmFinalFailure
		if errors.Is(err, publish.ErrValidation) || errors.Is(err, publish.ErrTasksIncomplete) {
			occasion = alarmValidation
		}
		c.notify(ctx, occasion, current.term, err.Error())
		return err
	}

	c.notifyAfterSuccess(ctx, current, before)
	return nil
}

// readBefore 读本轮开始前的状态：上一份快照的学期与最近一轮运行状态。
func (c *Collector) readBefore(ctx context.Context) (roundBefore, error) {
	var before roundBefore

	release, err := c.tracker.CurrentRelease(ctx)
	// 一次都没发布成功时没有 current，这不是故障
	if err == nil {
		before.term = release.Term
		before.hasRelease = true
	} else if !errors.Is(err, store.ErrNoRelease) {
		return before, err
	}

	status, found, err := c.tracker.LatestRunStatus(ctx)
	if err != nil {
		return before, err
	}
	before.status = status
	before.hasRun = found
	return before, nil
}

// prepare 跑采集开始前的三步：登录、读学期、采样周历并整体替换 term_week，再读齐本轮目录。
func (c *Collector) prepare(ctx context.Context) (*round, error) {
	// 登录是一切上游请求的前提：没有会话时任何请求都只会拿到登录页
	if err := c.session.Login(ctx); err != nil {
		return nil, fmt.Errorf("登录教务系统失败: %w", err)
	}

	term, err := c.readTerm(ctx)
	if err != nil {
		return nil, err
	}

	totalWeeks, err := c.writeCalendar(ctx, term)
	if err != nil {
		return nil, err
	}
	return c.loadRound(ctx, term, totalWeeks)
}

// readTerm 读当前学期编号：这一页的正文交给 calendar.ParseTerm（需求 1.1）。
func (c *Collector) readTerm(ctx context.Context) (string, error) {
	response, err := c.requestWithLogin(ctx, func() (fetch.Response, error) { return c.client.TermPage(ctx) })
	if err != nil {
		return "", err
	}
	term, err := calendar.ParseTerm(response.Body)
	// 认不出学期编号就必须失败：猜一个学期会把整轮数据写到别的学期上
	if err != nil {
		return "", fmt.Errorf("读取当前学期失败: %w", err)
	}
	return term, nil
}

// writeCalendar 用 Asia/Shanghai 的当天采样一次周历页，解析周次文本并整体写入 term_week。
// 返回学期总周数；解析失败时不写半截周历（需求 1.2、1.3、1.4）。
func (c *Collector) writeCalendar(ctx context.Context, term string) (int, error) {
	today := c.now().In(c.location)
	response, err := c.requestWithLogin(ctx, func() (fetch.Response, error) {
		return c.client.CalendarPage(ctx, today)
	})
	if err != nil {
		return 0, err
	}

	sample, err := calendar.ParseSample(response.Body)
	// 取不到「第 X 周 / 共 Y 周」就没有可靠的学期起点，本轮失败
	if err != nil {
		return 0, fmt.Errorf("周次采样失败: %w", err)
	}
	weeks, err := calendar.BuildWeeks(term, today, sample)
	if err != nil {
		return 0, fmt.Errorf("生成周历失败: %w", err)
	}
	if err := c.tracker.ReplaceTermWeeks(ctx, term, weeks); err != nil {
		return 0, err
	}
	c.logger.Info("周历已更新", "term", term, "weeks", sample.Total, "sample_week", sample.Week)
	return sample.Total, nil
}

// loadRound 读齐本轮要用的目录：字典版本与符号、状态表、节次轴，以及教学楼白名单。
func (c *Collector) loadRound(ctx context.Context, term string, totalWeeks int) (*round, error) {
	dictVersion, err := c.tracker.LatestDictVersion(ctx)
	if err != nil {
		return nil, err
	}
	axisVersion, err := c.tracker.LatestAxisVersion(ctx)
	if err != nil {
		return nil, err
	}
	symbols, err := c.tracker.DictSymbols(ctx, dictVersion)
	if err != nil {
		return nil, err
	}
	states, err := c.tracker.DictStates(ctx, dictVersion)
	if err != nil {
		return nil, err
	}
	axis, err := c.tracker.AxisNodes(ctx, axisVersion)
	if err != nil {
		return nil, err
	}
	buildings, err := c.tracker.BuildingWhitelist(ctx)
	if err != nil {
		return nil, err
	}

	return &round{
		term:        term,
		dictVersion: dictVersion,
		axisVersion: axisVersion,
		totalWeeks:  totalWeeks,
		today:       dateOf(c.now().In(c.location)),
		axis:        axis,
		symbols:     symbols,
		states:      states,
		buildings:   buildings,
	}, nil
}

// collect 跑采集阶段：先各周矩阵（任务状态机接管断点与重试），再按楼取聚合矩阵并补明细任务，
// 最后把明细任务跑到头。前一步被定为整轮失败时后面不开始（设计文档 Architecture）。
func (c *Collector) collect(ctx context.Context, current *round) error {
	worker := &roundWorker{collector: c, round: current}
	runner := sync.NewRunner(c.tracker, c.session, worker, c.now)

	if err := runner.Run(ctx, current.term, weekNumbers(current.totalWeeks)); err != nil {
		// 整轮停止与单任务失败都在这里：本轮不发布，明细阶段也不开始
		return err
	}

	// 聚合矩阵不在任务表里（sync_task 只有周矩阵与明细两类），它的失败由入口显式收尾
	if err := c.enqueueCellDetails(ctx, current); err != nil {
		return c.failRun(ctx, err)
	}

	runID, err := c.tracker.currentRunID()
	if err != nil {
		return err
	}
	return runner.Resume(ctx, runID)
}

// enqueueCellDetails 按楼取一次周次留空的聚合矩阵，只把有内容的格子补成明细任务（需求 4.1）。
func (c *Collector) enqueueCellDetails(ctx context.Context, current *round) error {
	runID, err := c.tracker.currentRunID()
	if err != nil {
		return err
	}

	for _, building := range current.buildings {
		response, err := c.requestWithLogin(ctx, func() (fetch.Response, error) {
			return c.client.AggregateMatrix(ctx, fetch.WeekMatrixParams{Term: current.term, BuildingID: building.Jxlbh})
		})
		if err != nil {
			return err
		}
		matrix, err := clean.Parse(response.Body, current.axis, current.symbols)
		// 表头大节或矩阵结构变了：和任务里同一类失败一样停止本轮，不发布
		if err != nil {
			return err
		}
		// 明细行的外键指向 room，聚合矩阵里的房间也要先落目录
		if err := c.upsertRooms(ctx, current, building, matrix); err != nil {
			return err
		}
		cells, err := clean.OccupiedCells(matrix, current.axis)
		if err != nil {
			return err
		}
		// 同一格只下一次钻：任务表的唯一索引保证重复建任务被跳过
		if err := c.tracker.EnqueueSyncTasks(ctx, runID, sync.CellDetailTasks(building.Jxlbh, cells)); err != nil {
			return err
		}
		c.logger.Info("已补齐明细任务", "building", building.Name, "cells", len(cells))
	}
	return nil
}

// publishRound 校验并发布本轮候选集：校验不过或事务失败都保留原 current（需求 6.1、6.2）。
func (c *Collector) publishRound(ctx context.Context, current *round) error {
	runID, err := c.tracker.currentRunID()
	if err != nil {
		return err
	}

	result, err := c.publisher.Publish(ctx, runID, current.candidate())
	if err != nil {
		return err
	}
	c.logger.Info("本轮发布完成",
		"release_id", result.ReleaseID,
		"rooms", result.Rooms,
		"observations", result.Observations,
		"details", result.Details,
		"unknown_cells", result.UnknownCells,
		"composite_cells", result.CompositeCells)
	return nil
}

// notifyAfterSuccess 发两种只该出现一次的通知：学期切换与连续失败后的首次恢复。
func (c *Collector) notifyAfterSuccess(ctx context.Context, current *round, before roundBefore) {
	// 学期换了：整张周历与观测都换到新学期的口径，管理员需要知道
	if before.hasRelease && before.term != current.term {
		c.notify(ctx, alarmTermChanged, current.term, fmt.Sprintf("学期从 %s 切换为 %s", before.term, current.term))
	}
	// 上一轮是失败的、这一轮成功：连续失败后的首次恢复只通知一次
	if before.hasRun && before.status == store.RunStatusFailed {
		c.notify(ctx, alarmRecovered, current.term, "上一轮失败后本轮已成功发布")
	}
}

// notify 发一条告警：告警发不出去只记日志，不改本轮的业务结论。
func (c *Collector) notify(ctx context.Context, occasion, term, reason string) {
	if err := c.alerter.Notify(ctx, occasion, term, reason, c.now()); err != nil {
		c.logger.Error("发送告警失败", "occasion", occasion, "error", err)
	}
}

// notifyFailure 发一条「本轮失败」告警：白名单为空属于配置问题，本轮已经是 blocked，
// 不再按失败告警（设计文档的告警场合里没有 blocked 这一类）。
func (c *Collector) notifyFailure(ctx context.Context, term string, err error) {
	// 白名单为空是 blocked：状态已经写明原因，不重复通知
	if errors.Is(err, sync.ErrEmptyWhitelist) {
		c.logger.Warn("教学楼白名单为空，本轮已阻断", "error", err)
		return
	}
	c.notify(ctx, alarmFinalFailure, term, err.Error())
}

// failRun 把本轮显式标成 failed：聚合矩阵不在任务表里，它的失败只能由入口收尾。
func (c *Collector) failRun(ctx context.Context, reason error) error {
	runID, err := c.tracker.currentRunID()
	// 还没开轮次说明失败发生在建任务之前，本轮没有可收尾的记录
	if err != nil {
		return reason
	}
	if err := c.tracker.FinishSyncRun(ctx, runID, store.RunStatusFailed, reason.Error()); err != nil {
		return errors.Join(reason, fmt.Errorf("标记轮次失败出错: %w", err))
	}
	return reason
}

// requestWithLogin 发一次非任务型上游请求：会话失效时重新登录再试一次。
// 任务里的请求由状态机负责重新登录；学期页、周历页与聚合矩阵不在任务里，在这里补同一套处置。
func (c *Collector) requestWithLogin(ctx context.Context, request func() (fetch.Response, error)) (fetch.Response, error) {
	response, err := request()
	// 会话失效与请求本身无关：重新登录后原样重试一次，仍失败就交给调用方
	if errors.Is(err, fetch.ErrSessionExpired) {
		if loginErr := c.session.Login(ctx); loginErr != nil {
			return fetch.Response{}, fmt.Errorf("重新登录失败: %w", loginErr)
		}
		return request()
	}
	return response, err
}

// upsertRooms 把一份矩阵里的房间写进目录：所属楼取自返回它的这次请求。
// 同一 jsbh 出现在两栋楼时返回整轮停止错误，不挑一栋覆盖（需求 2.4、2.5）。
func (c *Collector) upsertRooms(ctx context.Context, current *round, building store.Building, matrix clean.Matrix) error {
	for _, row := range matrix.Rows {
		room := model.Room{
			ID:           row.Jsbh,
			Name:         row.Name,
			NameRaw:      row.NameRaw,
			BuildingID:   building.Jxlbh,
			BuildingName: building.Name,
			FirstSeen:    &current.today,
			LastSeen:     &current.today,
		}
		// 冲突说明同一个 jsbh 出现在两栋白名单楼的结果里：本轮失败，不发布
		if err := c.tracker.UpsertRoom(ctx, room); err != nil {
			if errors.Is(err, store.ErrRoomBuildingConflict) {
				return sync.StopRun(err)
			}
			return err
		}
		current.rooms = append(current.rooms, room)
	}
	return nil
}

// candidate 把本轮暂存的数据拼成发布候选集。
// StopErr 留空：整轮停止类失败发生时不会走到发布这一步。
func (r *round) candidate() publish.Candidate {
	return publish.Candidate{
		Term:        r.term,
		DictVersion: r.dictVersion,
		AxisVersion: r.axisVersion,
		TotalWeeks:  r.totalWeeks,
		Axis:        r.axis,
		Rooms:       r.rooms,
		Weeks:       r.weeks,
		Details:     r.details,
	}
}

// buildingOf 按楼编号取白名单里的那一条：任务只带 jxlbh，展示名要与它一起写进房间目录。
func (r *round) buildingOf(buildingID string) (store.Building, error) {
	for _, building := range r.buildings {
		// 找到就不再往后看：白名单里的 jxlbh 是唯一身份
		if building.Jxlbh == buildingID {
			return building, nil
		}
	}
	return store.Building{}, fmt.Errorf("教学楼 %s 不在本轮白名单里", buildingID)
}

// weekNumbers 返回第 1 周到总周数的周次序列：采全学期，周数来自周历采样而不是写死的值。
func weekNumbers(totalWeeks int) []int {
	weeks := make([]int, 0, totalWeeks)
	for week := 1; week <= totalWeeks; week++ {
		weeks = append(weeks, week)
	}
	return weeks
}

// dateOf 把时刻归一成只含年月日的日期：房间目录的首见与最近时间都是 DATE 列。
func dateOf(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}
