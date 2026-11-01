package stats

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// BatchWriter 把一批聚合后的统计增量落库；通常由 store.WriteStats 实现。
type BatchWriter interface {
	WriteStats(ctx context.Context, batch store.StatsBatch) error
}

// record 是一条已定格的记账：业务日期、小时、星期与匿名摘要都在入队时算好。
// 入队后不再读时钟，避免一批记账跨零点时被摊到两天（CP17）。
type record struct {
	day       dateKey // 业务日期
	hour      int     // 小时 0..23
	weekday   int     // 星期 1..7
	visitor   string  // 匿名摘要；空串表示本次不计 UV
	building  string  // 教学楼编号；空串表示本次没有楼维度
	room      string  // 房号；空串表示本次不是单房查询
	keyword   string  // 已归一化并截断的关键词；空串表示本次没有关键词
	startNode string  // 节次区间起点；空串表示本次没有节次区间
	endNode   string  // 节次区间终点
}

// Recorder 是访问统计的记账器：查询路径只做一次非阻塞入队，后台按批落库。
type Recorder struct {
	log        *slog.Logger
	writer     BatchWriter
	clock      func() time.Time
	hasher     Hasher
	queue      chan record
	batchSize  int
	flushEvery time.Duration
	dropped    atomic.Int64 // 队列满被丢弃的记账数，只在日志里周期性汇报

	done       chan struct{} // Run 退出时关闭，供 Wait 等待最后一批写完
	finishOnce sync.Once     // 保证 done 只关一次，Run 重复调用也不会 panic
}

// RecorderOption 调整记账器的时钟、盐与批量参数；不传时按默认口径工作。
type RecorderOption func(*Recorder)

// WithClock 注入时间来源，测试用假时钟覆盖 PV/UV 累加与日切。
func WithClock(clock func() time.Time) RecorderOption {
	return func(r *Recorder) {
		if clock != nil {
			r.clock = clock
		}
	}
}

// WithVisitorSalt 注入部署级附加盐；留空表示只按天加盐。
// 部署级盐来自 settings 的秘密管理（任务 2.2），本任务尚未落库，
// 因此 main 显式传 nil；接入 2.2 时只改装配处，摘要算法与表结构都不动。
func WithVisitorSalt(salt []byte) RecorderOption {
	return func(r *Recorder) { r.hasher = NewHasher(salt) }
}

// WithQueueSize 设置记账队列长度；<=0 时保持默认值。
func WithQueueSize(size int) RecorderOption {
	return func(r *Recorder) {
		if size <= 0 {
			return
		}
		r.queue = make(chan record, size)
	}
}

// WithBatchSize 设置一批最多的记账条数；<=0 时保持默认值。
func WithBatchSize(size int) RecorderOption {
	return func(r *Recorder) {
		if size > 0 {
			r.batchSize = size
		}
	}
}

// WithFlushInterval 设置无新记账时的强制落库间隔；<=0 时保持默认值。
func WithFlushInterval(interval time.Duration) RecorderOption {
	return func(r *Recorder) {
		if interval > 0 {
			r.flushEvery = interval
		}
	}
}

// NewRecorder 组装记账器；writer 为 nil 时 Record 变成空操作，便于未装配统计的部署。
func NewRecorder(log *slog.Logger, writer BatchWriter, opts ...RecorderOption) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	r := &Recorder{
		log:        log,
		writer:     writer,
		clock:      time.Now,
		hasher:     NewHasher(nil),
		queue:      make(chan record, defaultQueueSize),
		batchSize:  defaultBatchSize,
		flushEvery: defaultFlushInterval,
		done:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Record 投递一次记账并立即返回。
// 队列满时丢弃并计数，绝不阻塞查询路径：统计丢几条可以接受，查询被统计拖慢不行（CP18）。
func (r *Recorder) Record(visit Visit) {
	if r == nil || r.writer == nil {
		return
	}

	now := r.clock()
	local := now.In(shanghai)
	rec := record{
		day:       keyOfDay(local),
		hour:      local.Hour(),
		weekday:   isoWeekday(local),
		visitor:   r.hasher.Hash(now, visit.VisitorID, visit.IP),
		building:  NormalizeDimensionKey(visit.Building),
		room:      NormalizeDimensionKey(visit.RoomID),
		keyword:   NormalizeKeyword(visit.Keyword),
		startNode: strings.TrimSpace(visit.StartNode),
		endNode:   strings.TrimSpace(visit.EndNode),
	}

	select {
	case r.queue <- rec:
	default:
		r.reportDrop()
	}
}

// Run 消费记账队列并批量落库，直到 ctx 结束；退出前尽力写完手里的一批。
// 单批写入失败只记日志，循环继续，后续记账照常处理（CP18）。
func (r *Recorder) Run(ctx context.Context) {
	if r == nil {
		return
	}
	// 无论正常退出还是未装配 writer，都让 Wait 拿到终态，避免关闭流程永远挂住。
	defer r.finish()
	if r.writer == nil {
		return
	}

	ticker := time.NewTicker(r.flushEvery)
	defer ticker.Stop()

	pending := make([]record, 0, r.batchSize)
	for {
		select {
		case <-ctx.Done():
			// 关闭时不再接收新记账，但已入队的记录仍要尽力落库，否则统计会整段丢。
			pending = append(pending, r.drain()...)
			r.flushFinal(pending)
			return
		case rec := <-r.queue:
			pending = append(pending, rec)
			if len(pending) >= r.batchSize {
				// 失败的一批不重试：重试会造成重复计数，丢一批统计比虚高更可接受。
				r.flush(ctx, pending)
				pending = pending[:0]
			}
		case <-ticker.C:
			if len(pending) > 0 {
				r.flush(ctx, pending)
				pending = pending[:0]
			}
		}
	}
}

// reportDrop 记一次因队列满而丢弃的记账。
// 只在第一次与每满 100 次时打日志：持续过载时逐条打日志会把日志变成第二个瓶颈。
func (r *Recorder) reportDrop() {
	dropped := r.dropped.Add(1)
	if dropped == 1 || dropped%100 == 0 {
		r.log.Warn("访问统计队列已满，丢弃记账", "dropped_total", dropped)
	}
}

// drain 非阻塞地取出队列里剩余的全部记账。
func (r *Recorder) drain() []record {
	records := make([]record, 0, len(r.queue))
	for {
		select {
		case rec := <-r.queue:
			records = append(records, rec)
		default:
			return records
		}
	}
}

// flush 聚合并写出一批记账；失败只记日志，不向上返回（CP18）。
func (r *Recorder) flush(ctx context.Context, records []record) {
	if len(records) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := r.writer.WriteStats(writeCtx, buildBatch(records)); err != nil {
		r.log.Warn("访问统计写入失败", "err", err, "records", len(records))
	}
}

// Wait 等待 Run 退出：返回时最后一批记账已经尽力写完，调用方可以安全关池。
// 只能在 Run 启动之后调用；没有启动 Run 时没有可等的退出，会一直阻塞。
func (r *Recorder) Wait() {
	if r == nil {
		return
	}
	<-r.done
}

// finish 关闭 done，唤醒 Wait。
func (r *Recorder) finish() {
	r.finishOnce.Do(func() { close(r.done) })
}

// flushFinal 用不受 ctx 取消影响的上下文写完最后一批：退出前不再因为信号而丢数据。
func (r *Recorder) flushFinal(records []record) {
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	r.flush(ctx, records)
}

// buildBatch 把一批记账折成落库增量：
//   - PV 按 (day, hour) 累加；
//   - UV 按 (day, hour, 摘要) 去重，同批内同一访客同一小时只留一行（日 UV 由 SQL 再按日去重）；
//   - 楼、房、关键词、小节各按 (day, 键) 累加，节次区间命中的每个小节各计一次（Requirement 34.8）。
//
// 同一批内同一主键只输出一行：这样每条 upsert 都不会对同一行更新两次。
func buildBatch(records []record) store.StatsBatch {
	hours := make(map[hourKey]int, len(records))
	visitors := make(map[visitorKey]struct{}, len(records))
	buildings := make(map[dimKey]int)
	rooms := make(map[dimKey]int)
	keywords := make(map[dimKey]int)
	nodes := make(map[dimKey]int)

	for _, rec := range records {
		hours[hourKey{day: rec.day, hour: rec.hour, weekday: rec.weekday}]++
		if rec.visitor != "" {
			visitors[visitorKey{day: rec.day, hour: rec.hour, hash: rec.visitor}] = struct{}{}
		}
		if rec.building != "" {
			buildings[dimKey{day: rec.day, key: rec.building}]++
		}
		if rec.room != "" {
			rooms[dimKey{day: rec.day, key: rec.room}]++
		}
		if rec.keyword != "" {
			keywords[dimKey{day: rec.day, key: rec.keyword}]++
		}
		for _, node := range nodeRange(rec.startNode, rec.endNode) {
			nodes[dimKey{day: rec.day, key: node}]++
		}
	}

	return store.StatsBatch{
		Hours:     hourRows(hours),
		Visitors:  visitorRows(visitors),
		Buildings: dayHitRows(buildings),
		Rooms:     dayHitRows(rooms),
		Keywords:  keywordRows(keywords),
		Nodes:     nodeRows(nodes),
	}
}

// hourKey 是 PV 的聚合键；weekday 由日期决定，带上它只为落库时不必再算一次。
type hourKey struct {
	day     dateKey
	hour    int
	weekday int
}

// visitorKey 是 UV 去重键。
type visitorKey struct {
	day  dateKey
	hour int
	hash string
}

// dimKey 是维度日表的聚合键：楼编号、房号、关键词、小节编号共用。
type dimKey struct {
	day dateKey
	key string
}

// hourRows 把 PV 计数转成落库行，并按 (日期, 小时) 升序排列，便于测试与日志对照。
func hourRows(counts map[hourKey]int) []store.StatHourPV {
	rows := make([]store.StatHourPV, 0, len(counts))
	for key, pv := range counts {
		rows = append(rows, store.StatHourPV{Day: key.day.time(), Hour: key.hour, Weekday: key.weekday, PV: pv})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.Before(rows[j].Day)
		}
		return rows[i].Hour < rows[j].Hour
	})
	return rows
}

// visitorRows 把 UV 去重键转成落库行。
func visitorRows(keys map[visitorKey]struct{}) []store.StatVisitorKey {
	rows := make([]store.StatVisitorKey, 0, len(keys))
	for key := range keys {
		rows = append(rows, store.StatVisitorKey{Day: key.day.time(), Hour: key.hour, Hash: key.hash})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.Before(rows[j].Day)
		}
		if rows[i].Hour != rows[j].Hour {
			return rows[i].Hour < rows[j].Hour
		}
		return rows[i].Hash < rows[j].Hash
	})
	return rows
}

// dayHitRows 把楼或房的命中计数转成落库行。
func dayHitRows(counts map[dimKey]int) []store.StatDayHits {
	rows := make([]store.StatDayHits, 0, len(counts))
	for key, hits := range counts {
		rows = append(rows, store.StatDayHits{Day: key.day.time(), Key: key.key, Hits: hits})
	}
	sortDayHits(rows)
	return rows
}

// keywordRows 把关键词命中计数转成落库行。
func keywordRows(counts map[dimKey]int) []store.StatKeywordHits {
	rows := make([]store.StatKeywordHits, 0, len(counts))
	for key, hits := range counts {
		rows = append(rows, store.StatKeywordHits{Day: key.day.time(), Keyword: key.key, Hits: hits})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.Before(rows[j].Day)
		}
		return rows[i].Keyword < rows[j].Keyword
	})
	return rows
}

// nodeRows 把小节命中计数转成落库行。
func nodeRows(counts map[dimKey]int) []store.StatNodeHits {
	rows := make([]store.StatNodeHits, 0, len(counts))
	for key, hits := range counts {
		rows = append(rows, store.StatNodeHits{Day: key.day.time(), Node: key.key, Hits: hits})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.Before(rows[j].Day)
		}
		return rows[i].Node < rows[j].Node
	})
	return rows
}

// sortDayHits 按 (日期, 键) 升序排列维度命中行。
func sortDayHits(rows []store.StatDayHits) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Day.Equal(rows[j].Day) {
			return rows[i].Day.Before(rows[j].Day)
		}
		return rows[i].Key < rows[j].Key
	})
}
