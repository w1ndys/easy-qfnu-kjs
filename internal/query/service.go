// 本文件属于 business 层：编排查询用例——解析日历、读存储、组装结果。
// 响应形状以 docs/contract/api.v2.md 为准，本层只产出结构化数据，不碰 HTTP。

package query

import (
	"context"
	"errors"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// NoDataError 表示没有可用的发布数据，HTTP 层映射成 40401。
type NoDataError struct {
	Message string // 给用户看的原因，直接进响应的 message
}

// Error 实现 error 接口。
func (e NoDataError) Error() string {
	return e.Message
}

// AvailabilityItem 是空教室结果里的一间房。
type AvailabilityItem struct {
	ID   string // 房间身份 jsbh
	Name string // 规范化展示名
}

// DayItem 是全天状态结果里的一间房：12 个小节的状态键。
type DayItem struct {
	ID       string                    // 房间身份 jsbh
	Name     string                    // 规范化展示名
	Statuses map[string]model.StateKey // 节次 '01'..'12' → 状态语义键
}

// QueryResult 是一次查询的结果，两种视图共用一个结构，由调用方按视图取用。
type QueryResult struct {
	Total        int                // 命中的房间总数（分页前）
	Availability []AvailabilityItem // 空教室视图结果，day 视图为空
	Day          []DayItem          // 全天状态视图结果，availability 视图为空
	Date         time.Time          // 解析出的目标日期
	Week         int                // 目标日期所在教学周次，不在教学周为 0
	Weekday      int                // 目标日期是星期几，1=周一 … 7=周日
}

// MetaState 是 meta 里对某个状态的说明。
type MetaState struct {
	Label     string // 状态中文名，前端直接展示
	Available bool   // 是否算可用，只有 free 与 fully_free 为 true
}

// MetaNode 是 meta 里的一个节次节点及其所属大节。
type MetaNode struct {
	Code  string // 节次编号 '01'..'12'
	Block string // 所属大节编码，如 '0102'
}

// MetaWeek 是 meta 里的逐周信息。
type MetaWeek struct {
	Week          int        // 教学周次
	Monday        time.Time  // 该周周一的绝对日期
	Published     bool       // 当前版本里是否有这一周的观测
	LastSuccessAt *time.Time // 该周所在成功 release 的生成时间，两版都没有时为 nil
	Stale         bool       // 是否过期：距该周最近成功超过 8 天，或最近一次采集失败
}

// MetaTerm 是 meta 里的一个学期及其周次列表。
type MetaTerm struct {
	Term       string     // 学期标识，如 2025-2026-1
	TotalWeeks int        // 总周数
	Weeks      []MetaWeek // 逐周信息，按周次升序
}

// MetaResult 是 /api/meta 的数据载荷。
type MetaResult struct {
	ReleaseID   string               // 当前发布版本 id
	GeneratedAt time.Time            // 当前版本的发布时间
	DictVersion int                  // 该版本数据用的字典版本
	AxisVersion int                  // 该版本数据用的节次轴版本
	States      map[string]MetaState // 状态键 → 状态说明
	Blocks      []string             // 大节编码顺序，如 0102、030405
	Nodes       []MetaNode           // 12 个节次节点及其所属大节
	Terms       []MetaTerm           // 学期及其逐周新鲜度
}

// Service 是查询用例入口。
type Service struct {
	store *store.Store     // 数据层入口
	now   func() time.Time // 时钟，测试可注入固定时间
	loc   *time.Location   // 业务时区，固定 Asia/Shanghai
}

// NewService 组装查询服务；now 可注入，便于测试固定时间。
func NewService(dataStore *store.Store, loc *time.Location, now func() time.Time) *Service {
	// 调用方没给时钟就用系统时间，生产路径走这里
	if now == nil {
		now = time.Now
	}
	return &Service{store: dataStore, now: now, loc: loc}
}

// Context 返回今天落在哪个学期、哪一周、星期几。
func (s *Service) Context(ctx context.Context) (model.CalendarContext, error) {
	weeks, err := s.store.ListTermWeeks(ctx)
	if err != nil {
		return model.CalendarContext{}, err
	}
	return ResolveCalendar(weeks, s.today()), nil
}

// Query 按视图返回空教室或全天状态。
func (s *Service) Query(ctx context.Context, params Params) (QueryResult, error) {
	release, err := s.store.CurrentRelease(ctx)
	// 一次都没发布过时，查询无从下手，按“无数据”返回
	if errors.Is(err, store.ErrNoRelease) {
		return QueryResult{}, NoDataError{Message: "没有已发布的数据"}
	}
	if err != nil {
		return QueryResult{}, err
	}

	weeks, err := s.store.ListTermWeeks(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	calendar := ResolveCalendar(weeks, s.targetDate(params.DateOffset))
	result := QueryResult{Date: calendar.Date, Week: calendar.Week, Weekday: calendar.Weekday}

	// 不在教学周不是错误：返回空结果，前端按 /api/context 的 in_calendar 提示
	if !calendar.InCalendar {
		return result, nil
	}

	published, err := s.store.WeekPublished(ctx, release.ID, calendar.Term, calendar.Week)
	if err != nil {
		return QueryResult{}, err
	}
	// 在教学周内但这一周没采到，属于“数据未收录”
	if !published {
		return QueryResult{}, NoDataError{Message: "该日期没有已发布数据"}
	}

	// 视图只有两个取值，参数层已校验；availability 之外的都走全天状态
	if params.View == ViewAvailability {
		return s.availability(ctx, result, release, calendar, params)
	}
	return s.day(ctx, result, release, calendar, params)
}

// Meta 组装数据版本、字典、节次轴与逐周新鲜度。
func (s *Service) Meta(ctx context.Context) (MetaResult, error) {
	release, err := s.store.CurrentRelease(ctx)
	// 没有任何发布版本时面板没有数据可展示，按“无数据”返回
	if errors.Is(err, store.ErrNoRelease) {
		return MetaResult{}, NoDataError{Message: "没有已发布的数据"}
	}
	if err != nil {
		return MetaResult{}, err
	}

	dictStates, err := s.store.DictStates(ctx, release.DictVersion)
	if err != nil {
		return MetaResult{}, err
	}
	axisNodes, err := s.store.AxisNodes(ctx, release.AxisVersion)
	if err != nil {
		return MetaResult{}, err
	}
	terms, err := s.store.ListTerms(ctx)
	if err != nil {
		return MetaResult{}, err
	}
	weeks, err := s.store.ListTermWeeks(ctx)
	if err != nil {
		return MetaResult{}, err
	}
	published, err := s.publishedIndex(ctx, release)
	if err != nil {
		return MetaResult{}, err
	}
	runFailed, err := s.latestRunFailed(ctx)
	if err != nil {
		return MetaResult{}, err
	}

	states := make(map[string]MetaState, len(dictStates))
	for _, state := range dictStates {
		states[string(state.StateKey)] = MetaState{Label: state.Label, Available: state.Available}
	}
	blocks := make([]string, 0, 5)
	nodes := make([]MetaNode, 0, len(axisNodes))
	seenBlock := make(map[string]bool, 5)
	for _, node := range axisNodes {
		nodes = append(nodes, MetaNode{Code: node.Node, Block: node.Block})
		// 大节只登记一次，顺序跟着节次顺序走
		if !seenBlock[node.Block] {
			seenBlock[node.Block] = true
			blocks = append(blocks, node.Block)
		}
	}

	return MetaResult{
		ReleaseID:   release.ID,
		GeneratedAt: release.GeneratedAt,
		DictVersion: release.DictVersion,
		AxisVersion: release.AxisVersion,
		States:      states,
		Blocks:      blocks,
		Nodes:       nodes,
		Terms:       s.buildTerms(s.now().In(s.loc), terms, weeks, published, runFailed),
	}, nil
}

// availability 查询区间内每一节都可用的房间。
func (s *Service) availability(ctx context.Context, result QueryResult, release model.Release, calendar model.CalendarContext, params Params) (QueryResult, error) {
	total, rooms, err := s.store.Availability(ctx, store.AvailabilityFilter{
		ReleaseID: release.ID,
		Term:      calendar.Term,
		Week:      calendar.Week,
		Weekday:   calendar.Weekday,
		StartNode: params.StartNode,
		EndNode:   params.EndNode,
		NodeCount: params.NodeCount,
		Keyword:   params.Keyword,
		Limit:     params.Limit,
		Offset:    params.Offset,
	})
	if err != nil {
		return QueryResult{}, err
	}

	result.Total = total
	result.Availability = make([]AvailabilityItem, 0, len(rooms))
	for _, room := range rooms {
		result.Availability = append(result.Availability, AvailabilityItem{ID: room.ID, Name: room.Name})
	}
	return result, nil
}

// day 查询当日每个房间的 12 小节状态。
func (s *Service) day(ctx context.Context, result QueryResult, release model.Release, calendar model.CalendarContext, params Params) (QueryResult, error) {
	total, cells, err := s.store.DayStatuses(ctx, store.DayFilter{
		ReleaseID: release.ID,
		Term:      calendar.Term,
		Week:      calendar.Week,
		Weekday:   calendar.Weekday,
		Keyword:   params.Keyword,
		Limit:     params.Limit,
		Offset:    params.Offset,
	})
	if err != nil {
		return QueryResult{}, err
	}

	result.Total = total
	result.Day = make([]DayItem, 0, len(cells)/12+1)
	positions := make(map[string]int, len(cells)/12+1)
	for _, cell := range cells {
		position, exists := positions[cell.RoomID]
		// 第一次遇到这间房时开一条新记录，后续节次补进同一条
		if !exists {
			position = len(result.Day)
			positions[cell.RoomID] = position
			result.Day = append(result.Day, DayItem{
				ID:       cell.RoomID,
				Name:     cell.Name,
				Statuses: make(map[string]model.StateKey, 12),
			})
		}
		result.Day[position].Statuses[cell.Node] = model.StateKey(cell.StateKey)
	}
	return result, nil
}

// publishedIndex 记录 current 与 previous 各自有观测的周，以及各自的生成时间。
// published 只认 current；上一版有而当前没有时，仍保留上一版时间给 last_success_at。
func (s *Service) publishedIndex(ctx context.Context, current model.Release) (publishedIndex, error) {
	index := publishedIndex{
		current:   map[weekKey]bool{},
		previous:  map[weekKey]bool{},
		currentAt: current.GeneratedAt,
	}

	currentWeeks, err := s.store.PublishedWeeks(ctx, current.ID)
	if err != nil {
		return publishedIndex{}, err
	}
	for _, week := range currentWeeks {
		index.current[weekKey{term: week.Term, week: week.Week}] = true
	}

	previous, err := s.store.PreviousRelease(ctx)
	// 只保留下两版的策略下，第一次发布后还没有上一版，这是正常情况
	if errors.Is(err, store.ErrNoRelease) {
		return index, nil
	}
	if err != nil {
		return publishedIndex{}, err
	}

	index.previousAt = previous.GeneratedAt
	previousWeeks, err := s.store.PublishedWeeks(ctx, previous.ID)
	if err != nil {
		return publishedIndex{}, err
	}
	for _, week := range previousWeeks {
		index.previous[weekKey{term: week.Term, week: week.Week}] = true
	}
	return index, nil
}

// latestRunFailed 判断最近一轮采集是否失败；没有运行记录，或状态不是 failed，都不算失败。
func (s *Service) latestRunFailed(ctx context.Context) (bool, error) {
	status, found, err := s.store.LatestRunStatus(ctx)
	if err != nil {
		return false, err
	}
	// 还没跑过采集时没有“失败”可言，避免把空库误判成过期
	if !found {
		return false, nil
	}
	// 只认 failed；running、blocked、success 都不是失败，不能用“不是 success”代替
	return status == "failed", nil
}

// buildTerms 把学期周历、已发布周与新鲜度组装成 meta 的逐周信息。
func (s *Service) buildTerms(now time.Time, terms []model.Term, weeks []model.TermWeek, published publishedIndex, runFailed bool) []MetaTerm {
	result := make([]MetaTerm, 0, len(terms))
	for _, term := range terms {
		metaTerm := MetaTerm{Term: term.Term, TotalWeeks: term.TotalWeeks, Weeks: make([]MetaWeek, 0, len(weeks))}
		for _, week := range weeks {
			// 周历里有别的学期，这里只取当前学期的周
			if week.Term != term.Term {
				continue
			}
			isPublished, lastSuccess := published.lookup(week.Term, week.Week)
			metaTerm.Weeks = append(metaTerm.Weeks, MetaWeek{
				Week:          week.Week,
				Monday:        dateOnly(week.Monday),
				Published:     isPublished,
				LastSuccessAt: lastSuccess,
				Stale:         WeekStale(now, isPublished, lastSuccess, runFailed),
			})
		}
		result = append(result, metaTerm)
	}
	return result
}

// today 返回服务端时钟在配置时区下的今天。
func (s *Service) today() time.Time {
	return dateOnly(s.now().In(s.loc))
}

// targetDate 按 date_offset 天数得到目标日期。
func (s *Service) targetDate(offset int) time.Time {
	return dateOnly(s.now().In(s.loc).AddDate(0, 0, offset))
}

// weekKey 是 (学期, 周次) 组合，用作已发布周的键。
type weekKey struct {
	term string // 学期标识
	week int    // 教学周次
}

// publishedIndex 是两版发布的周集合与生成时间。
type publishedIndex struct {
	current    map[weekKey]bool // current 版本里已有观测的 (学期, 周次)
	previous   map[weekKey]bool // previous 版本里已有观测的 (学期, 周次)
	currentAt  time.Time        // current 版本的生成时间
	previousAt time.Time        // previous 版本的生成时间
}

// lookup 返回 current 是否有该周观测，以及该周所在成功 release 的生成时间。
func (p publishedIndex) lookup(term string, week int) (bool, *time.Time) {
	key := weekKey{term: term, week: week}
	// 当前版本有这一周才算已发布，时间用当前版本生成时间
	if p.current[key] {
		at := p.currentAt
		return true, &at
	}
	// 上一版有、当前没有：不算已发布，最近成功时间仍取上一版生成时间
	if p.previous[key] {
		at := p.previousAt
		return false, &at
	}
	// 两版都没有这一周，没有成功时间，也不做过期判定
	return false, nil
}
