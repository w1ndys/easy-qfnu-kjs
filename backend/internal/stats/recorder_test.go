package stats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestRecorderCountsPVByHour 覆盖 PV 按 (day, hour) 累加，且星期取自业务日期（CP17）。
func TestRecorderCountsPVByHour(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 30, 0, 0, shanghai) // 周日
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	records := recordAndDrain(t, recorder, Visit{VisitorID: "v1"}, Visit{VisitorID: "v2"})
	// 跨到下一个小时：同一访客也要单独占一条小时 UV 行。
	now = now.Add(30 * time.Minute)
	records = append(records, recordAndDrain(t, recorder, Visit{VisitorID: "v1"})...)

	batch := buildBatch(records)
	if got := pvByHour(batch); got[8] != 2 || got[9] != 1 {
		t.Fatalf("期望 8 时 2 次、9 时 1 次，实际 %v", got)
	}
	if len(batch.Hours) != 2 {
		t.Fatalf("期望 2 个小时行，实际 %d 行", len(batch.Hours))
	}
	for _, row := range batch.Hours {
		if row.Weekday != 7 {
			t.Fatalf("2026-11-01 是周日，期望 weekday=7，实际 %d", row.Weekday)
		}
	}
	if len(batch.Visitors) != 3 {
		t.Fatalf("期望 3 条小时 UV 行（8 时两条、9 时一条），实际 %d 条", len(batch.Visitors))
	}
}

// TestRecorderDedupesUVPerDayAndHour 覆盖同一访客同一小时只留一行、跨小时各留一行（CP17）。
func TestRecorderDedupesUVPerDayAndHour(t *testing.T) {
	now := time.Date(2026, 11, 2, 8, 10, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	records := recordAndDrain(t, recorder, Visit{VisitorID: "same"})
	now = now.Add(40 * time.Minute) // 同一小时
	records = append(records, recordAndDrain(t, recorder, Visit{VisitorID: "same"})...)

	batch := buildBatch(records)
	if len(batch.Visitors) != 1 {
		t.Fatalf("同一访客同一小时应只留一行，实际 %d 行", len(batch.Visitors))
	}
	if got := pvByHour(batch)[8]; got != 2 {
		t.Fatalf("期望 8 时 PV=2，实际 %d", got)
	}

	now = now.Add(30 * time.Minute) // 09:20，另一个小时
	records = append(records, recordAndDrain(t, recorder, Visit{VisitorID: "same"})...)
	batch = buildBatch(records)
	if len(batch.Visitors) != 2 {
		t.Fatalf("跨小时应各留一行，实际 %d 行", len(batch.Visitors))
	}
	if batch.Visitors[0].Hash != batch.Visitors[1].Hash {
		t.Fatal("同一自然日内摘要应稳定，日 UV 才能按摘要去重")
	}
	if batch.Visitors[0].Hour == batch.Visitors[1].Hour {
		t.Fatal("两行应落在不同小时")
	}
}

// TestRecorderRestartsUVAfterDayRollover 覆盖日切：换日后同一访客重新计一次，且摘要随日盐变化（CP17）。
func TestRecorderRestartsUVAfterDayRollover(t *testing.T) {
	now := time.Date(2026, 11, 2, 23, 59, 0, 0, shanghai) // 周一
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	records := recordAndDrain(t, recorder, Visit{VisitorID: "same"})
	now = time.Date(2026, 11, 3, 0, 1, 0, 0, shanghai) // 周二
	records = append(records, recordAndDrain(t, recorder, Visit{VisitorID: "same"})...)

	batch := buildBatch(records)
	if len(batch.Visitors) != 2 {
		t.Fatalf("换日应重新计一次，期望 2 行，实际 %d 行", len(batch.Visitors))
	}
	if batch.Visitors[0].Day.Equal(batch.Visitors[1].Day) {
		t.Fatal("两行应落在不同业务日期")
	}
	if batch.Visitors[0].Hash == batch.Visitors[1].Hash {
		t.Fatal("摘要按天加盐，换日应得到不同摘要")
	}
	days := map[int]bool{}
	for _, row := range batch.Visitors {
		days[row.Day.Day()] = true
	}
	if !days[2] || !days[3] {
		t.Fatalf("期望 11 月 2 日与 3 日各一行，实际 %v", days)
	}
	// 跨零点也换了星期：周一 1、周二 2。
	weekdays := map[int]bool{}
	for _, row := range batch.Hours {
		weekdays[row.Weekday] = true
	}
	if !weekdays[1] || !weekdays[2] {
		t.Fatalf("期望跨日后的小时行分别是周一与周二，实际 %v", weekdays)
	}
}

// TestRecorderCountsDimensionsAndNodes 覆盖楼、房、关键词与节次区间各写日表（Requirement 34.5、34.8）。
func TestRecorderCountsDimensionsAndNodes(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	records := recordAndDrain(t, recorder,
		Visit{Building: "Q10", Keyword: " Ｆ１２６ ", StartNode: "02", EndNode: "04"},
		Visit{Building: "Q10", RoomID: "0899"},
		Visit{}, // 没有任何维度：只计 PV
	)

	batch := buildBatch(records)
	if len(batch.Buildings) != 1 || batch.Buildings[0].Key != "Q10" || batch.Buildings[0].Hits != 2 {
		t.Fatalf("期望教学楼 Q10 命中 2 次，实际 %+v", batch.Buildings)
	}
	if len(batch.Rooms) != 1 || batch.Rooms[0].Key != "0899" || batch.Rooms[0].Hits != 1 {
		t.Fatalf("期望教室 0899 命中 1 次，实际 %+v", batch.Rooms)
	}
	if len(batch.Keywords) != 1 || batch.Keywords[0].Keyword != "f126" || batch.Keywords[0].Hits != 1 {
		t.Fatalf("期望关键词归一化为 f126，实际 %+v", batch.Keywords)
	}
	if len(batch.Nodes) != 3 {
		t.Fatalf("02–04 应命中 3 个小节，实际 %d 个", len(batch.Nodes))
	}
	for i, node := range batch.Nodes {
		want := []string{"02", "03", "04"}[i]
		if node.Node != want || node.Hits != 1 {
			t.Fatalf("第 %d 个小节期望 %s 命中 1 次，实际 %+v", i+1, want, node)
		}
	}
	if got := pvByHour(batch)[10]; got != 3 {
		t.Fatalf("期望 10 时 PV=3，实际 %d", got)
	}
	if len(batch.Visitors) != 0 {
		t.Fatalf("没有访客标识与 IP 时不应写 UV 行，实际 %d 行", len(batch.Visitors))
	}
}

// TestRecorderClampsDimensionKeys 覆盖楼与房维度键的夹取（Requirement 34.5）：
// 超长键来自公开请求，必须在记账侧截到 MaxDimensionRunes，
// 否则一条超长键就足以撞上统计日表的 btree 索引上限。
func TestRecorderClampsDimensionKeys(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	longBuilding := strings.Repeat("楼", MaxDimensionRunes+16)
	longRoom := strings.Repeat("房", MaxDimensionRunes+16)
	records := recordAndDrain(t, recorder,
		Visit{
			Building:  " " + longBuilding + " ",
			RoomID:    longRoom,
			Keyword:   "F126",
			StartNode: "01",
			EndNode:   "02",
		},
		Visit{Building: "   ", RoomID: "\t"}, // 纯空白按「没有该维度」处理
	)

	batch := buildBatch(records)
	if len(batch.Buildings) != 1 {
		t.Fatalf("超长楼编号应只留一条截断后的键，实际 %+v", batch.Buildings)
	}
	if got := utf8.RuneCountInString(batch.Buildings[0].Key); got != MaxDimensionRunes {
		t.Fatalf("楼编号应截到 %d 个字符，实际 %d 个字符", MaxDimensionRunes, got)
	}
	if !strings.HasPrefix(longBuilding, batch.Buildings[0].Key) {
		t.Fatalf("截断应保留原键前缀，实际 %q", batch.Buildings[0].Key)
	}
	if batch.Buildings[0].Hits != 1 {
		t.Fatalf("期望楼维度命中 1 次，实际 %d 次", batch.Buildings[0].Hits)
	}

	if len(batch.Rooms) != 1 {
		t.Fatalf("超长房号应只留一条截断后的键，实际 %+v", batch.Rooms)
	}
	if got := utf8.RuneCountInString(batch.Rooms[0].Key); got != MaxDimensionRunes {
		t.Fatalf("房号应截到 %d 个字符，实际 %d 个字符", MaxDimensionRunes, got)
	}

	// 同一批的关键词与节次不能被超长维度键拖下水。
	if len(batch.Keywords) != 1 || batch.Keywords[0].Keyword != "f126" || batch.Keywords[0].Hits != 1 {
		t.Fatalf("同批关键词维度应正常落库，实际 %+v", batch.Keywords)
	}
	if len(batch.Nodes) != 2 {
		t.Fatalf("01–02 应命中 2 个小节，实际 %d 个", len(batch.Nodes))
	}
}

// TestRecorderWaitFlushesLastBatchOnClose 覆盖关闭收尾（Requirement 34.1）：
// ctx 结束后 Wait 返回时，队列里最后一批记账必须已经写出。
func TestRecorderWaitFlushesLastBatchOnClose(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	writer := newFakeWriter()
	recorder := NewRecorder(quietLogger(), writer,
		WithClock(func() time.Time { return now }),
		WithQueueSize(8),
		// 批次阈值拉大、定时器拉长：这批只可能由退出时的收尾写入落库。
		WithBatchSize(64),
		WithFlushInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go recorder.Run(ctx)

	recorder.Record(Visit{VisitorID: "v1", Building: "Q10", RoomID: "0899"})
	cancel()
	recorder.Wait()

	if got := writer.batchCount(); got != 1 {
		t.Fatalf("关闭时应把最后一批写出，实际 %d 批", got)
	}
	batch := writer.lastBatch()
	if got := pvByHour(batch)[10]; got != 1 {
		t.Fatalf("最后一批的 PV 应为 1，实际 %d", got)
	}
	if len(batch.Buildings) != 1 || batch.Buildings[0].Key != "Q10" {
		t.Fatalf("最后一批应带上楼维度 Q10，实际 %+v", batch.Buildings)
	}
	if len(batch.Rooms) != 1 || batch.Rooms[0].Key != "0899" {
		t.Fatalf("最后一批应带上房维度 0899，实际 %+v", batch.Rooms)
	}
}

// TestNormalizeKeywordFoldsWidthCaseAndTruncates 覆盖关键词归一化与截断（Requirement 34.5）。
func TestNormalizeKeywordFoldsWidthCaseAndTruncates(t *testing.T) {
	long := strings.Repeat("教", MaxKeywordRunes+8)
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "全角转半角并转小写", raw: "  Ｆ１２６  ", want: "f126"},
		{name: "表意空格与全角数字", raw: "综合楼　３０２", want: "综合楼 302"},
		{name: "折叠重复空白", raw: "综合   楼", want: "综合 楼"},
		{name: "空串", raw: "", want: ""},
		{name: "纯空白", raw: "   ", want: ""},
		{name: "超长截断", raw: long, want: strings.Repeat("教", MaxKeywordRunes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeKeyword(tc.raw); got != tc.want {
				t.Fatalf("NormalizeKeyword(%q) 期望 %q，实际 %q", tc.raw, tc.want, got)
			}
		})
	}
}

// TestVisitorHashPrefersBrowserIDAndFallsBackToIP 覆盖匿名标识的选择与加日盐（Requirement 34.4）。
func TestVisitorHashPrefersBrowserIDAndFallsBackToIP(t *testing.T) {
	hasher := NewHasher(nil)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, shanghai)

	fromID := hasher.Hash(day, "browser-7f3c", "203.0.113.9")
	fromIP := hasher.Hash(day, "", "203.0.113.9")
	fromIPBlankID := hasher.Hash(day, "   ", "203.0.113.9")

	if len(fromID) != 64 {
		t.Fatalf("期望 32 字节摘要的十六进制串，实际长度 %d", len(fromID))
	}
	if fromID == fromIP {
		t.Fatal("有前端标识时应优先使用标识，不能落成 IP 摘要")
	}
	if fromIP != fromIPBlankID {
		t.Fatal("空白标识应按缺失处理，退回 IP 摘要")
	}
	if hasher.Hash(day, "browser-7f3c", "") != fromID {
		t.Fatal("有前端标识时 IP 不应参与摘要")
	}
	if hasher.Hash(day.AddDate(0, 0, 1), "browser-7f3c", "") == fromID {
		t.Fatal("摘要按天加盐，换日应得到不同摘要")
	}
	if hasher.Hash(day, "", "") != "" {
		t.Fatal("既无标识也无 IP 时不应产生摘要")
	}
	if hasher.Hash(day, strings.Repeat("x", maxVisitorIDLength+1), "") == hasher.Hash(day, strings.Repeat("x", 8), "") {
		t.Fatal("超长标识应按缺失处理，而不是参与摘要")
	}
}

// TestRecorderKeepsNoRawIdentifiers 覆盖 CP16：统计增量里没有 IP、UA、来源页与查询原文。
func TestRecorderKeepsNoRawIdentifiers(t *testing.T) {
	const (
		visitorID = "browser-7f3c"
		ip        = "203.0.113.9"
		rawQuery  = "ＳＺ　３０２"
	)
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(4))

	records := recordAndDrain(t, recorder, Visit{
		VisitorID: visitorID,
		IP:        ip,
		Building:  "Q10",
		RoomID:    "0899",
		Keyword:   rawQuery,
		StartNode: "01",
		EndNode:   "02",
	})
	batch := buildBatch(records)

	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("统计增量序列化失败: %v", err)
	}
	for _, forbidden := range []string{visitorID, ip, rawQuery, "User-Agent", "Referer"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("统计增量不应包含 %q，实际内容 %s", forbidden, payload)
		}
	}
	// 关键词只留归一化形态。
	if len(batch.Keywords) != 1 || batch.Keywords[0].Keyword != "sz 302" {
		t.Fatalf("期望关键词落库为归一化形态 sz 302，实际 %+v", batch.Keywords)
	}
	for _, visitor := range batch.Visitors {
		if len(visitor.Hash) != 64 {
			t.Fatalf("访客行只应存摘要，实际 %q", visitor.Hash)
		}
	}
}

// TestRecorderDropsWhenQueueFullWithoutBlocking 覆盖队列满时不阻塞查询（CP18）。
func TestRecorderDropsWhenQueueFullWithoutBlocking(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(1))

	// 没有 Run 消费者：只有第一条能入队，其余必须立刻丢弃而不是把调用方挂住。
	recorder.Record(Visit{VisitorID: "v1"})
	recorder.Record(Visit{VisitorID: "v2"})
	recorder.Record(Visit{VisitorID: "v3"})

	if got := recorder.dropped.Load(); got != 2 {
		t.Fatalf("期望丢弃 2 条，实际 %d 条", got)
	}
	if got := len(recorder.queue); got != 1 {
		t.Fatalf("期望队列里留下 1 条，实际 %d 条", got)
	}
}

// TestRecorderFlushesQueuedVisits 覆盖后台批量落库：入队后由 Run 写出一批。
func TestRecorderFlushesQueuedVisits(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	writer := newFakeWriter()
	recorder := NewRecorder(quietLogger(), writer,
		WithClock(func() time.Time { return now }),
		WithQueueSize(8),
		WithBatchSize(1),
		// 拉长定时器，让本用例只由批量阈值触发写入。
		WithFlushInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		recorder.Run(ctx)
	}()

	recorder.Record(Visit{VisitorID: "v1", Building: "Q10"})
	writer.waitForFlush(t)

	batch := writer.lastBatch()
	if got := pvByHour(batch)[10]; got != 1 {
		t.Fatalf("期望 10 时 PV=1，实际 %d", got)
	}
	if len(batch.Buildings) != 1 || batch.Buildings[0].Key != "Q10" {
		t.Fatalf("期望楼维度写入 Q10，实际 %+v", batch.Buildings)
	}

	cancel()
	<-done
}

// TestRecorderRunSurvivesWriteFailure 覆盖记账失败后后台循环继续工作（CP18）。
func TestRecorderRunSurvivesWriteFailure(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	writer := newFakeWriter()
	writer.setError(errors.New("db down"))
	recorder := NewRecorder(quietLogger(), writer,
		WithClock(func() time.Time { return now }),
		WithQueueSize(8),
		WithBatchSize(1),
		WithFlushInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		recorder.Run(ctx)
	}()

	recorder.Record(Visit{VisitorID: "v1"})
	writer.waitForFlush(t)

	// 写入失败不应让循环退出：修好库之后的下一条记账仍会被写出。
	writer.setError(nil)
	recorder.Record(Visit{VisitorID: "v2"})
	writer.waitForFlush(t)

	select {
	case <-done:
		t.Fatal("写入失败后 Run 不应退出")
	default:
	}
	if got := writer.batchCount(); got != 2 {
		t.Fatalf("期望尝试写入 2 批，实际 %d 批", got)
	}

	cancel()
	<-done
}

// TestRecorderWriteFailureOnlyLogs 覆盖 CP18：写入失败只写日志，不向上抛错。
func TestRecorderWriteFailureOnlyLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	writer := newFakeWriter()
	writer.setError(errors.New("db down"))
	recorder := NewRecorder(logger, writer,
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	// 直接调 flush：失败路径本身不返回错误，日志在调用返回前就已写完，用例读日志不带竞态。
	recorder.flush(context.Background(), recordAndDrain(t, recorder, Visit{VisitorID: "v1"}))
	if !strings.Contains(logs.String(), "访问统计写入失败") {
		t.Fatalf("写入失败应记日志，实际日志 %q", logs.String())
	}
	if got := writer.batchCount(); got != 1 {
		t.Fatalf("期望尝试写入 1 批，实际 %d 批", got)
	}
}

// TestBuildBatchKeepsDuplicateVisitorsOncePerHour 覆盖同一批内同一访客同一小时只输出一行。
func TestBuildBatchKeepsDuplicateVisitorsOncePerHour(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, shanghai)
	recorder := NewRecorder(quietLogger(), newFakeWriter(),
		WithClock(func() time.Time { return now }), WithQueueSize(8))

	records := recordAndDrain(t, recorder,
		Visit{VisitorID: "v1"}, Visit{VisitorID: "v1"}, Visit{VisitorID: "v1"})

	batch := buildBatch(records)
	if len(batch.Visitors) != 1 {
		t.Fatalf("同一批内同一访客同一小时应只留一行，实际 %d 行", len(batch.Visitors))
	}
	if got := pvByHour(batch)[10]; got != 3 {
		t.Fatalf("PV 应按次累加为 3，实际 %d", got)
	}
}

// 编译期断言：测试替身满足对外接口。
var (
	_ BatchWriter = (*fakeWriter)(nil)
	_ Deleter     = (*fakeDeleter)(nil)
)
