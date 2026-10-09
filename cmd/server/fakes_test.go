package main

// 本文件是采集器入口的测试夹具：内存版数据层、假上游、假会话与记录告警的假发送方。
// 不连数据库、不访问网络：HTTP 交给假 RoundTripper，正文都是手写的最小片段，
// 真实教务 HTML 与真实 Cookie 不进 git。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// testBaseURL 是假上游的根地址：假传输不解析主机名，只用它拼路径。
const testBaseURL = "http://upstream.test"

// testTerm 是测试用的学期编号。
const testTerm = "2026-2027-1"

// testTermBody 是学期页正文片段：按 docs/upstream.md 第 2 节记录的 YYYY-YYYY-N 格式手写。
const testTermBody = `当前学期：2026-2027-1`

// testCalendarBody 是周历页正文片段：第 1 周、共 2 周，两周的用例足够断言管线顺序。
const testCalendarBody = `第1周/2周`

// testSecretCookie 是假会话里的 Cookie：断言告警正文里不能出现它。
const testSecretCookie = "JSESSIONID=test-secret-cookie"

// testBlocks 是契约种子里的 5 个大节编码，表头按天重复。
var testBlocks = []string{"0102", "030405", "0607", "0809", "101112"}

// fakeClock 是假时钟：等待只把时间往前推，测试不真等 500 毫秒。
type fakeClock struct {
	now time.Time // 当前假时间
}

// Now 返回当前假时间。
func (c *fakeClock) Now() time.Time { return c.now }

// Sleep 把假时间往前推一段时间。
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// testLogger 造一个写进丢弃缓冲的日志器：用例只关心编排，不关心日志输出。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSession 是假会话：记登录次数，Cookie 由测试给定。
type fakeSession struct {
	logins    int    // 登录次数
	cookie    string // 当前 Cookie 头
	presetErr error  // 预设的登录失败原因，非空时按它失败
}

// Login 记一次登录；presetErr 非空时按它失败，用来覆盖登录失败的分支。
func (s *fakeSession) Login(ctx context.Context) error {
	s.logins++
	return s.presetErr
}

// Cookie 返回测试给定的 Cookie。
func (s *fakeSession) Cookie() string { return s.cookie }

// recordingNotifier 是假发送方：把告警正文记下来，供断言内容不敏感。
type recordingNotifier struct {
	texts []string // 收到的告警正文
}

// Send 记一条告警正文。
func (n *recordingNotifier) Send(ctx context.Context, text string) error {
	n.texts = append(n.texts, text)
	return nil
}

// upstreamCall 是一次被假上游记下的请求。
type upstreamCall struct {
	path   string     // 请求路径
	method string     // HTTP 方法
	form   url.Values // 表单或查询串参数
	cookie string     // 请求头里的 Cookie
}

// fakeUpstream 是假上游：按请求内容给出预置正文，并记下请求顺序。
type fakeUpstream struct {
	calls   []upstreamCall                        // 已记录的请求，按发生顺序
	respond func(call upstreamCall) (int, string) // 按请求给出状态码与正文
}

// RoundTrip 记录一次请求并交给 respond 决定响应。
func (u *fakeUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	call, err := readCall(req)
	if err != nil {
		return nil, err
	}
	u.calls = append(u.calls, call)

	status, body := u.respond(call)
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// readCall 读出一次请求的路径、方法与参数：GET 的参数在查询串里，POST 的在表单里。
func readCall(req *http.Request) (upstreamCall, error) {
	call := upstreamCall{path: req.URL.Path, method: req.Method, cookie: req.Header.Get("Cookie")}
	// GET 没有请求体，参数只能从查询串读
	if req.Method == http.MethodGet {
		form, err := url.ParseQuery(req.URL.RawQuery)
		if err != nil {
			return upstreamCall{}, err
		}
		call.form = form
		return call, nil
	}

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return upstreamCall{}, err
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		return upstreamCall{}, err
	}
	call.form = form
	return call, nil
}

// defaultUpstream 造一个假上游：学期页、周历页、周矩阵、聚合矩阵与明细都给出可用的正文。
// 一栋楼、一间房、两个教学周：够断言顺序，又不把用例铺大。
func defaultUpstream() *fakeUpstream {
	upstream := &fakeUpstream{respond: func(call upstreamCall) (int, string) {
		switch call.path {
		case fetch.TermPath:
			return http.StatusOK, testTermBody
		case fetch.CalendarPath:
			return http.StatusOK, testCalendarBody
		case fetch.WeekMatrixPath:
			// zc 留空的是聚合矩阵：只让它有一个有内容的格子，用来挑出要下钻的明细
			if call.form.Get("zc") == "" {
				return http.StatusOK, buildMatrixBody(testRoom{jsbh: "r-1", nameRaw: "F126(90/0)", firstCell: "◆"})
			}
			return http.StatusOK, buildMatrixBody(testRoom{jsbh: "r-1", nameRaw: "F126(90/0)"})
		case fetch.CellDetailPath:
			return http.StatusOK, buildDetailBody()
		}
		return http.StatusNotFound, ""
	}}
	return upstream
}

// testRoom 是夹具里的一间房：身份、原始房名与周一第一个大节的原文。
type testRoom struct {
	jsbh      string // 房间身份
	nameRaw   string // 房名，含容量片段
	firstCell string // 周一 0102 格的原文；留空表示空闲
}

// buildMatrixBody 拼一份最小矩阵：表头用契约的 5 个大节重复 7 天，每间房一行。
func buildMatrixBody(rooms ...testRoom) string {
	return buildMatrixBodyWithBlocks(testBlocks, rooms...)
}

// buildMatrixBodyWithBlocks 拼一份表头用给定块集合的矩阵：表头块变化用它造结构失败。
func buildMatrixBodyWithBlocks(blocks []string, rooms ...testRoom) string {
	var body strings.Builder
	body.WriteString(`<html><body><table id="dataList"><tr>`)
	for day := 0; day < 7; day++ {
		for _, code := range blocks {
			body.WriteString(`<td tdvalue="`)
			body.WriteString(code)
			body.WriteString(`"></td>`)
		}
	}
	body.WriteString(`</tr>`)
	for _, room := range rooms {
		body.WriteString(`<tr jsbh="`)
		body.WriteString(room.jsbh)
		body.WriteString(`"><td>`)
		body.WriteString(room.nameRaw)
		body.WriteString(`</td>`)
		// 35 格按天分组：只有周一 0102 格可能带内容，其余留空表示空闲
		for index := 0; index < 35; index++ {
			cell := ""
			if index == 0 {
				cell = room.firstCell
			}
			body.WriteString("<td>")
			body.WriteString(cell)
			body.WriteString("</td>")
		}
		body.WriteString(`</tr>`)
	}
	body.WriteString(`</table></body></html>`)
	return body.String()
}

// buildDetailBody 拼一份最小弹窗：只放教室状态、课程与周次三项。
func buildDetailBody() string {
	fields := []struct {
		label string // 字段名
		value string // 字段值
	}{
		{"教室状态", "正常上课"},
		{"课程", "西方史学史"},
		{"周次", "1-2"},
	}
	var body strings.Builder
	body.WriteString(`<html><body><center id="alldiv"><table id="tab1">`)
	for _, field := range fields {
		body.WriteString(`<tr><td>`)
		body.WriteString(field.label)
		body.WriteString(`</td><td></td><td>`)
		body.WriteString(field.value)
		body.WriteString(`</td></tr>`)
	}
	body.WriteString(`</table></center></body></html>`)
	return body.String()
}

// testAxis 造契约种子里的节次轴：5 大节共 12 小节，按展示顺序。
func testAxis() []model.AxisNode {
	blocks := []struct {
		code  string // 大节编码
		nodes int    // 该大节覆盖几个小节
	}{
		{"0102", 2},
		{"030405", 3},
		{"0607", 2},
		{"0809", 2},
		{"101112", 3},
	}

	nodes := make([]model.AxisNode, 0, 12)
	ordinal := 0
	for blockOrdinal, block := range blocks {
		for index := 0; index < block.nodes; index++ {
			ordinal++
			nodes = append(nodes, model.AxisNode{
				AxisVersion:  1,
				Node:         fmt.Sprintf("%02d", ordinal),
				Ordinal:      ordinal,
				Block:        block.code,
				BlockOrdinal: blockOrdinal + 1,
			})
		}
	}
	return nodes
}

// testSymbols 造契约种子里 dict_version 1 的符号表，码点原样。
func testSymbols() []model.DictSymbol {
	return []model.DictSymbol{
		{Symbol: "◆", StateKey: model.StateClass},
		{Symbol: "空闲", StateKey: model.StateFree},
		{Symbol: "完全空闲", StateKey: model.StateFullyFree},
	}
}

// testStates 造契约种子里 dict_version 1 的状态表：明细里的中文状态名按它换语义键。
func testStates() []model.DictState {
	return []model.DictState{
		{StateKey: model.StateClass, Label: "正常上课"},
		{StateKey: model.StateFree, Label: "空闲", Available: true},
	}
}

// fakeTaskRow 是一条内存任务：任务本身加它当前的状态。
type fakeTaskRow struct {
	task     store.SyncTask // 任务本身
	status   string         // pending / done / failed
	attempts int            // 上游请求次数
	message  string         // 失败原因
}

// fakeStore 是内存版数据层：语义与 internal/store 一致，测试不连数据库。
type fakeStore struct {
	buildings  []store.Building      // 白名单教学楼
	symbols    []model.DictSymbol    // 符号表
	states     []model.DictState     // 状态表
	axis       []model.AxisNode      // 节次轴
	terms      []model.TermWeek      // 最近一次写入的周历
	rooms      map[string]model.Room // 房间目录
	alerts     map[string]int        // 告警去重键 → 次数
	release    model.Release         // 当前已发布版本
	hasRelease bool                  // 是否已有 current
	lastStatus string                // 本轮开始前的最近一轮状态
	hasRun     bool                  // 本轮开始前是否已有轮次
	batches    []store.PublishBatch  // 收到的发布批次
	publishErr error                 // 发布事务的预设失败

	runID     int64          // 本轮轮次 id
	runStatus string         // 本轮轮次状态
	runError  string         // 本轮最近错误
	tasks     []*fakeTaskRow // 本轮任务，按建立顺序
	nextID    int64          // 任务自增
}

// newFakeStore 造一份可用的内存数据层：一栋楼、字典与节轴种子、空房间目录。
func newFakeStore() *fakeStore {
	return &fakeStore{
		buildings: []store.Building{{Jxlbh: "jxlbh-1", Name: "综合楼"}},
		symbols:   testSymbols(),
		states:    testStates(),
		axis:      testAxis(),
		rooms:     make(map[string]model.Room),
		alerts:    make(map[string]int),
	}
}

// StartSyncRun 开一轮；已经有 running 轮次时与真实存储一样拒绝。
func (s *fakeStore) StartSyncRun(ctx context.Context, term string) (int64, error) {
	// 同一时刻至多一轮：已有 running 时拒绝，不建新轮次
	if s.runStatus == store.RunStatusRunning {
		return 0, store.ErrRunInProgress
	}
	s.runID++
	s.runStatus = store.RunStatusRunning
	s.runError = ""
	s.tasks = nil
	return s.runID, nil
}

// BlockSyncRun 把本轮标成 blocked 并记下原因。
func (s *fakeStore) BlockSyncRun(ctx context.Context, runID int64, reason string) error {
	s.runStatus = store.RunStatusBlocked
	s.runError = reason
	return nil
}

// FinishSyncRun 结束本轮，写下状态与最近错误。
func (s *fakeStore) FinishSyncRun(ctx context.Context, runID int64, status string, lastError string) error {
	s.runStatus = status
	s.runError = lastError
	return nil
}

// BuildingWhitelist 返回测试配置的白名单。
func (s *fakeStore) BuildingWhitelist(ctx context.Context) ([]store.Building, error) {
	return s.buildings, nil
}

// EnqueueSyncTasks 写入任务：同一轮里同样的单元只留一条。
func (s *fakeStore) EnqueueSyncTasks(ctx context.Context, runID int64, tasks []store.SyncTask) error {
	for _, task := range tasks {
		// 续跑时会重新建一次清单，重复的单元不能再建一条
		if s.hasTask(task) {
			continue
		}
		s.nextID++
		task.ID = s.nextID
		s.tasks = append(s.tasks, &fakeTaskRow{task: task, status: "pending"})
	}
	return nil
}

// hasTask 判断本轮是否已经有同一个任务单元。
func (s *fakeStore) hasTask(task store.SyncTask) bool {
	for _, row := range s.tasks {
		// 六个字段一起构成任务单元，任何一个不同都是另一个任务
		if row.task.Kind == task.Kind && row.task.BuildingID == task.BuildingID &&
			row.task.Week == task.Week && row.task.RoomID == task.RoomID &&
			row.task.Weekday == task.Weekday && row.task.Block == task.Block {
			return true
		}
	}
	return false
}

// NextPendingSyncTask 取第一条待办任务：done 与 failed 的都跳过。
func (s *fakeStore) NextPendingSyncTask(ctx context.Context, runID int64) (store.SyncTask, bool, error) {
	for _, row := range s.tasks {
		// 只有 pending 的才是待办
		if row.status == "pending" {
			return row.task, true, nil
		}
	}
	return store.SyncTask{}, false, nil
}

// MarkSyncTaskDone 把任务标成完成。
func (s *fakeStore) MarkSyncTaskDone(ctx context.Context, taskID int64, attempts int) error {
	row := s.findTask(taskID)
	row.status = "done"
	row.attempts = attempts
	return nil
}

// MarkSyncTaskFailed 把任务标成失败并记下请求次数与原因。
func (s *fakeStore) MarkSyncTaskFailed(ctx context.Context, taskID int64, attempts int, message string) error {
	row := s.findTask(taskID)
	row.status = "failed"
	row.attempts = attempts
	row.message = message
	return nil
}

// SyncProgress 统计本轮的完成数与总数。
func (s *fakeStore) SyncProgress(ctx context.Context, runID int64) (int, int, error) {
	done := 0
	for _, row := range s.tasks {
		// 完成数只数 done，失败的任务仍算未完成
		if row.status == "done" {
			done++
		}
	}
	return done, len(s.tasks), nil
}

// findTask 按 id 找任务：测试里 id 一定存在。
func (s *fakeStore) findTask(taskID int64) *fakeTaskRow {
	for _, row := range s.tasks {
		// 任务 id 由存储分配，唯一
		if row.task.ID == taskID {
			return row
		}
	}
	panic(fmt.Sprintf("测试任务 %d 不存在", taskID))
}

// ReplaceTermWeeks 整体替换某学期的周历。
func (s *fakeStore) ReplaceTermWeeks(ctx context.Context, term string, weeks []model.TermWeek) error {
	s.terms = weeks
	return nil
}

// UpsertRoom 写入房间目录；同一 jsbh 换楼时与真实存储一样拒绝。
func (s *fakeStore) UpsertRoom(ctx context.Context, room model.Room) error {
	if existing, ok := s.rooms[room.ID]; ok && existing.BuildingID != "" && existing.BuildingID != room.BuildingID {
		return fmt.Errorf("%w: %s", store.ErrRoomBuildingConflict, room.ID)
	}
	s.rooms[room.ID] = room
	return nil
}

// LatestDictVersion 返回测试用的字典版本。
func (s *fakeStore) LatestDictVersion(ctx context.Context) (int, error) { return 1, nil }

// LatestAxisVersion 返回测试用的节次轴版本。
func (s *fakeStore) LatestAxisVersion(ctx context.Context) (int, error) { return 1, nil }

// DictSymbols 返回测试用的符号表。
func (s *fakeStore) DictSymbols(ctx context.Context, dictVersion int) ([]model.DictSymbol, error) {
	return s.symbols, nil
}

// DictStates 返回测试用的状态表。
func (s *fakeStore) DictStates(ctx context.Context, dictVersion int) ([]model.DictState, error) {
	return s.states, nil
}

// AxisNodes 返回测试用的节次轴。
func (s *fakeStore) AxisNodes(ctx context.Context, axisVersion int) ([]model.AxisNode, error) {
	return s.axis, nil
}

// CurrentRelease 返回当前已发布版本；还没有发布过时返回 store.ErrNoRelease。
func (s *fakeStore) CurrentRelease(ctx context.Context) (model.Release, error) {
	// 一次都没发布成功时没有 current，这不是故障
	if !s.hasRelease {
		return model.Release{}, store.ErrNoRelease
	}
	return s.release, nil
}

// LatestRunStatus 返回本轮开始前的最近一轮状态。
func (s *fakeStore) LatestRunStatus(ctx context.Context) (string, bool, error) {
	return s.lastStatus, s.hasRun, nil
}

// RecordAlert 落一条告警并返回它是不是首次出现。
func (s *fakeStore) RecordAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error) {
	s.alerts[dedupeKey]++
	return s.alerts[dedupeKey] == 1, nil
}

// PublishRelease 记下发布批次；预设失败时原样返回错误，current 保持不动。
func (s *fakeStore) PublishRelease(ctx context.Context, batch store.PublishBatch) error {
	// 事务失败的用例只看 current 有没有变
	if s.publishErr != nil {
		return s.publishErr
	}
	s.batches = append(s.batches, batch)
	s.release = batch.Release
	s.hasRelease = true
	return nil
}

// testRig 是一套测试装置：采集器加它的假数据层、假上游、假会话与假发送方。
type testRig struct {
	collector *Collector         // 被测的采集器
	data      *fakeStore         // 假数据层
	upstream  *fakeUpstream      // 假上游
	session   *fakeSession       // 假会话
	alerts    *recordingNotifier // 记录告警正文的假发送方
	clock     *fakeClock         // 假时钟
}

// newTestRig 装配一套不连数据库、不访问网络的采集器。
func newTestRig(t *testing.T) *testRig {
	t.Helper()

	data := newFakeStore()
	upstream := defaultUpstream()
	session := &fakeSession{cookie: testSecretCookie}
	clock := &fakeClock{now: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	alerts := &recordingNotifier{}

	collector, err := newCollector(collectorConfig{
		store:      data,
		baseURL:    testBaseURL,
		session:    session,
		httpClient: &http.Client{Transport: upstream},
		logger:     testLogger(),
		location:   time.UTC,
		now:        clock.Now,
		sleep:      clock.Sleep,
		notifier:   alerts,
	})
	if err != nil {
		t.Fatalf("装配采集器失败: %v", err)
	}
	return &testRig{collector: collector, data: data, upstream: upstream, session: session, alerts: alerts, clock: clock}
}

// callLabels 把被记下的请求翻成可读标签，用来断言管线的固定顺序。
func callLabels(calls []upstreamCall) []string {
	labels := make([]string, 0, len(calls))
	for _, call := range calls {
		switch call.path {
		case fetch.TermPath:
			labels = append(labels, "term")
		case fetch.CalendarPath:
			labels = append(labels, "calendar")
		case fetch.WeekMatrixPath:
			// zc 留空的是聚合矩阵，带具体周次的是周矩阵
			if call.form.Get("zc") == "" {
				labels = append(labels, "aggregate")
				continue
			}
			labels = append(labels, "week:"+call.form.Get("zc"))
		case fetch.CellDetailPath:
			labels = append(labels, "detail")
		default:
			labels = append(labels, call.path)
		}
	}
	return labels
}

// sameLabels 比较两次请求序列是否一致：定时与手动必须跑同一套任务。
func sameLabels(left, right []string) bool {
	// 长度不同就直接不同，避免下面按下标越界
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		// 逐项比较顺序与内容
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
