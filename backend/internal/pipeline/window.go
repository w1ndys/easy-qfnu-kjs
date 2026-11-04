package pipeline

// windowWeeks 是采集窗口最多覆盖的周数：当前周加之后三周，闭区间共 4 周。
// 窗口上限来自教研口径（Requirement 1.1），不是可以随手调大的批量参数。
const windowWeeks = 4

// Window 是采集窗口的闭区间 [Start, End]，Start 与 End 都是教学周序号。
// 零值 Window{} 表示空窗：Start 小于 1 或 End 小于 Start 都视为没有可查的周，
// 调用方据此把该轮置为「跳过」而不是自己再拼一遍判定。
type Window struct {
	Start int // 窗口首周，即当前教学周 C
	End   int // 窗口末周，min(学期总周数 Y, C+3)
}

// WindowFor 由当前教学周 currentWeek 与学期总周数 totalWeeks 求采集窗口
// `currentWeek..min(totalWeeks, currentWeek+3)`（Requirement 1.1）。
//
// 当前周小于 1、总周数小于当前周或学期没有可用周时返回零值 Window：
// 这三种输入都拼不出合法的闭区间，属于「空窗」，由 Plan 统一置为 skipped。
// currentWeek+3 在 int 上溢出时回落到 totalWeeks：只有 currentWeek 接近 int
// 上限（C > MaxInt-3）才会溢出，而能取到的 totalWeeks 不超过 MaxInt，所以
// C+3 在数学上仍大于 Y，min(Y, C+3) 本来就是 Y，回落结果依旧正确。
func WindowFor(currentWeek, totalWeeks int) Window {
	if currentWeek < 1 || totalWeeks < currentWeek {
		return Window{}
	}

	end := currentWeek + windowWeeks - 1
	// end < currentWeek 说明加法溢出；end > totalWeeks 说明还没到四周就放假了。
	// 两种情况都截到学期最后一周，得到 min(totalWeeks, currentWeek+3)。
	if end < currentWeek || end > totalWeeks {
		end = totalWeeks
	}

	return Window{Start: currentWeek, End: end}
}

// Empty 报告窗口是否没有可采的周。
// 零值 Window{} 与 Start>End 的区间都算空，调用方不需要再判断字段。
func (w Window) Empty() bool {
	return w.Start < 1 || w.End < w.Start
}

// Weeks 按升序返回窗口内的周次，闭区间 [Start, End]；空窗返回 nil。
// 返回的是新切片，调用方可以随意持有，不影响 Window 本身。
func (w Window) Weeks() []int {
	if w.Empty() {
		return nil
	}

	// 防御导出方法的宽窗口：Plan 已用跨度守卫挡住，但直接调用仍可能让
	// make 因容量超运行时上限 panic。窗口最长就是 windowWeeks 周。
	if w.End-w.Start+1 > windowWeeks {
		return nil
	}

	weeks := make([]int, 0, w.End-w.Start+1)
	// 终止条件写成「已到末周」而不是 week <= w.End：End 可能等于 math.MaxInt，
	// 那样 week++ 会回绕成 math.MinInt，循环永不结束。
	for week := w.Start; ; week++ {
		weeks = append(weeks, week)
		if week == w.End {
			break
		}
	}

	return weeks
}
