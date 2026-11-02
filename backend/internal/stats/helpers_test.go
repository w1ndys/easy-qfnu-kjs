package stats

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// quietLogger 返回丢弃输出的日志器，避免统计日志淹没测试输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeWriter 是统计写入替身：记录每批增量、可注入错误，并在每次写入后发一次信号。
// 信号让测试可以「等到写入发生」而不是 sleep，用例既确定又快。
type fakeWriter struct {
	mu      sync.Mutex
	batches []store.StatsBatch
	err     error
	flushes chan struct{}
}

// newFakeWriter 造一个带缓冲信号的写入替身。
func newFakeWriter() *fakeWriter {
	return &fakeWriter{flushes: make(chan struct{}, 32)}
}

// WriteStats 实现 BatchWriter。
func (w *fakeWriter) WriteStats(_ context.Context, batch store.StatsBatch) error {
	w.mu.Lock()
	w.batches = append(w.batches, batch)
	err := w.err
	w.mu.Unlock()

	// 非阻塞发信号：替身不能因为测试还没来得及读信号而卡住后台循环。
	select {
	case w.flushes <- struct{}{}:
	default:
	}
	return err
}

// setError 设置后续写入的返回值。
func (w *fakeWriter) setError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

// batchCount 返回已收到的批数。
func (w *fakeWriter) batchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.batches)
}

// lastBatch 返回最近一批增量；没有写入时返回零值。
func (w *fakeWriter) lastBatch() store.StatsBatch {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.batches) == 0 {
		return store.StatsBatch{}
	}
	return w.batches[len(w.batches)-1]
}

// waitForFlush 等下一次写入发生；超时视为用例失败。
func (w *fakeWriter) waitForFlush(t *testing.T) {
	t.Helper()
	select {
	case <-w.flushes:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待统计写入超时，已收到 %d 批", w.batchCount())
	}
}

// blockingWriter 是「写到一半」的写入替身：进入 WriteStats 后挂住，直到测试放行。
// 用来模拟「关闭信号到来时这一批已经在写」：用例先取消再放行，
// 断言写入仍然完成，且写上下文没有被父 ctx 的取消信号带走。
type blockingWriter struct {
	started chan struct{} // 第一次进入 WriteStats 时关闭
	release chan struct{} // 测试关闭它来放行
	once    sync.Once

	mu            sync.Mutex
	batches       []store.StatsBatch
	ctxErrAtWrite error // 写入完成时看到的 ctx 状态
}

// newBlockingWriter 造一个会挂住写入的替身。
func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

// WriteStats 实现 BatchWriter：先发「已开始」信号，再等测试放行。
func (w *blockingWriter) WriteStats(ctx context.Context, batch store.StatsBatch) error {
	w.once.Do(func() { close(w.started) })
	<-w.release

	w.mu.Lock()
	defer w.mu.Unlock()
	w.batches = append(w.batches, batch)
	w.ctxErrAtWrite = ctx.Err()
	return nil
}

// batchCount 返回已写完的批数。
func (w *blockingWriter) batchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.batches)
}

// lastBatch 返回最近写完的一批增量；没有写入时返回零值。
func (w *blockingWriter) lastBatch() store.StatsBatch {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.batches) == 0 {
		return store.StatsBatch{}
	}
	return w.batches[len(w.batches)-1]
}

// writeCtxErr 返回最近一次写入完成时写上下文的状态。
func (w *blockingWriter) writeCtxErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctxErrAtWrite
}

// fakeDeleter 是清理替身：记录每次删除的边界，可注入错误。
type fakeDeleter struct {
	mu      sync.Mutex
	cutoffs []time.Time
	deleted int64
	err     error
	calls   chan struct{}
}

// newFakeDeleter 造一个带缓冲信号的删除替身。
func newFakeDeleter() *fakeDeleter {
	return &fakeDeleter{calls: make(chan struct{}, 4)}
}

// DeleteStatsBefore 实现 Deleter。
func (d *fakeDeleter) DeleteStatsBefore(_ context.Context, cutoff time.Time) (int64, error) {
	d.mu.Lock()
	d.cutoffs = append(d.cutoffs, cutoff)
	deleted, err := d.deleted, d.err
	d.mu.Unlock()

	// 非阻塞发信号：清理间隔在用例里可能非常短，替身不能把循环卡在发送上。
	select {
	case d.calls <- struct{}{}:
	default:
	}
	return deleted, err
}

// lastCutoff 返回最近一次的清理边界；没有调用时返回零值。
func (d *fakeDeleter) lastCutoff() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cutoffs) == 0 {
		return time.Time{}
	}
	return d.cutoffs[len(d.cutoffs)-1]
}

// recordAndDrain 用真实记账路径投递若干访问，再把队列里的记账取出来供聚合断言。
func recordAndDrain(t *testing.T, recorder *Recorder, visits ...Visit) []record {
	t.Helper()
	for _, visit := range visits {
		recorder.Record(visit)
	}
	records := recorder.drain()
	if len(records) != len(visits) {
		t.Fatalf("队列里应有 %d 条记账，实际 %d 条", len(visits), len(records))
	}
	return records
}

// pvByHour 把小时行折成「小时 → PV」。
func pvByHour(batch store.StatsBatch) map[int]int {
	pv := make(map[int]int, len(batch.Hours))
	for _, row := range batch.Hours {
		pv[row.Hour] = row.PV
	}
	return pv
}
