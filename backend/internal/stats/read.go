package stats

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// FactsLookup 从存储层读出聚合所需的原始计数；通常绑定 store.ReadStatFacts 与连接池。
type FactsLookup func(ctx context.Context, windows store.StatWindows, topN int) (store.StatFacts, error)

// ReadParams 是读取访问统计的口径参数。
type ReadParams struct {
	// PeakDays 是星期 × 小时热力与峰值占比的窗口，只取 7 或 30（PRD「近 7 日与近 30 日切换」）。
	// 其它值按默认 7 天处理。
	PeakDays int
}

// Reader 读取访问统计并整形成大屏需要的形状。
type Reader struct {
	lookup FactsLookup // 原始计数读取，由装配层注入
}

// NewReader 组装读取侧；lookup 为 nil 时 Snapshot 返回错误而不是空数据。
func NewReader(lookup FactsLookup) *Reader {
	return &Reader{lookup: lookup}
}

// Snapshot 返回 PV/UV 三段口径、三张 Top 10、热力与峰值、逐日趋势与节次分布。
// 窗口一律由业务日期（Asia/Shanghai）推出，与查询路径共用同一个时钟来源。
func (r *Reader) Snapshot(ctx context.Context, now time.Time, params ReadParams) (Snapshot, error) {
	if r == nil || r.lookup == nil {
		return Snapshot{}, fmt.Errorf("访问统计读取未装配")
	}

	peakDays := normalizePeakDays(params.PeakDays)
	windows := buildWindows(now, peakDays)

	facts, err := r.lookup(ctx, windows, TopN)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		Traffic: Traffic{
			PV: Counts{Today: facts.Today.PV, D7: facts.D7.PV, D30: facts.D30.PV},
			UV: Counts{Today: facts.Today.UV, D7: facts.D7.UV, D30: facts.D30.UV},
		},
		Leaderboard:      leaderboard(facts),
		Peaks:            peaks(facts.Heatmap),
		Trend:            trend(facts.Days, windows.D30),
		NodeDistribution: nodeDistribution(facts.Nodes),
	}, nil
}

// buildWindows 由「今天」推出读取需要的四段窗口。
// 今日只含当天；近 7 日与近 30 日按闭区间含今天，所以偏移分别是 6 与 29 天。
func buildWindows(now time.Time, peakDays int) store.StatWindows {
	today := dayOf(now)
	return store.StatWindows{
		Today: store.StatRange{From: today, To: today},
		D7:    store.StatRange{From: today.AddDate(0, 0, -(LeaderboardDays - 1)), To: today},
		D30:   store.StatRange{From: today.AddDate(0, 0, -(TrendDays - 1)), To: today},
		Peak:  store.StatRange{From: today.AddDate(0, 0, -(peakDays - 1)), To: today},
	}
}

// normalizePeakDays 只接受 7 或 30，其它值按默认 7 天。
func normalizePeakDays(days int) int {
	if days == PeakDays30 {
		return PeakDays30
	}
	return DefaultPeakDays
}

// leaderboard 把存储层的榜单行转成对外形状；空榜返回空切片而不是 nil，避免 JSON 出现 null。
func leaderboard(facts store.StatFacts) Leaderboard {
	board := Leaderboard{
		Buildings: make([]BuildingHit, 0, len(facts.Buildings)),
		Rooms:     make([]RoomHit, 0, len(facts.Rooms)),
		Keywords:  make([]KeywordHit, 0, len(facts.Keywords)),
	}
	for _, building := range facts.Buildings {
		board.Buildings = append(board.Buildings, BuildingHit{
			BuildingID: building.ID,
			Name:       building.Name,
			Hits:       building.Hits,
		})
	}
	for _, room := range facts.Rooms {
		board.Rooms = append(board.Rooms, RoomHit{RoomID: room.ID, Name: room.Name, Hits: room.Hits})
	}
	for _, keyword := range facts.Keywords {
		board.Keywords = append(board.Keywords, KeywordHit{Text: keyword.Text, Hits: keyword.Hits})
	}
	return board
}

// peaks 把热力计数铺成 7 × 24 矩阵，并算出访问量最高的前三个时段（Requirement 34.6）。
// 占比的分母是窗口内总访问量，因此把整张热力图按格除以总数后，各格占比之和为 1；
// 峰值时段只是其中最大的三格，三者占比之和不等于 1。
func peaks(cells []store.StatHeatCell) Peaks {
	matrix := make([][]int, 7)
	for weekday := range matrix {
		matrix[weekday] = make([]int, 24)
	}

	total := 0
	for _, cell := range cells {
		// 脏数据不该让整块大屏 panic：越界格直接跳过。
		if cell.Weekday < 1 || cell.Weekday > 7 || cell.Hour < 0 || cell.Hour > 23 {
			continue
		}
		matrix[cell.Weekday-1][cell.Hour] += cell.PV
		total += cell.PV
	}

	return Peaks{Heatmap: matrix, Top: topPeaks(matrix, total)}
}

// peakCell 是排序用的临时格：对外形状里不输出单格计数，只输出占比。
type peakCell struct {
	weekday int
	hour    int
	pv      int
}

// topPeaks 取访问量最高的前三个时段，同分按星期、再按小时升序排，保证结果稳定。
func topPeaks(matrix [][]int, total int) []Peak {
	cells := make([]peakCell, 0, len(matrix)*24)
	for weekday, row := range matrix {
		for hour, pv := range row {
			// 没有访问的时段不进峰值榜：占比 0 的「峰值」没有意义。
			if pv <= 0 {
				continue
			}
			cells = append(cells, peakCell{weekday: weekday + 1, hour: hour, pv: pv})
		}
	}
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].pv != cells[j].pv {
			return cells[i].pv > cells[j].pv
		}
		if cells[i].weekday != cells[j].weekday {
			return cells[i].weekday < cells[j].weekday
		}
		return cells[i].hour < cells[j].hour
	})

	if len(cells) > PeakTopN {
		cells = cells[:PeakTopN]
	}
	top := make([]Peak, 0, len(cells))
	for _, cell := range cells {
		top = append(top, Peak{
			Weekday: cell.weekday,
			Hour:    cell.hour,
			Share:   float64(cell.pv) / float64(total),
		})
	}
	return top
}

// trend 把窗口内出现过的日补成逐日序列：缺数据的日补 0，保证趋势图连续（Requirement 34.7）。
// 窗口是近 30 日，末尾 7 项即近 7 日口径。
func trend(days []store.StatDayPV, window store.StatRange) []TrendPoint {
	byDay := make(map[string]int, len(days))
	for _, day := range days {
		byDay[day.Day.Format(time.DateOnly)] += day.PV
	}

	points := make([]TrendPoint, 0, TrendDays)
	for day := window.From; !day.After(window.To); day = day.AddDate(0, 0, 1) {
		key := day.Format(time.DateOnly)
		points = append(points, TrendPoint{Day: key, PV: byDay[key]})
	}
	return points
}

// nodeDistribution 把节次命中转成对外形状，保持存储层的小节升序；没有命中的小节不补 0。
func nodeDistribution(nodes []store.StatNodeHit) []NodeHit {
	hits := make([]NodeHit, 0, len(nodes))
	for _, node := range nodes {
		hits = append(hits, NodeHit{Node: node.Node, Hits: node.Hits})
	}
	return hits
}
