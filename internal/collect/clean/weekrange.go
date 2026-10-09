// 本文件属于 business 层：把占用明细的周次范围与时间标志解析成教学周次。
// 契约里的写法是「1-16」；时间标志区分单周与双周。
// 读不出周次时返回 ErrWeekRange：调用方必须让那些周保持未知，绝不按空闲处理。
// 依据 specs/collector-full-sync/requirements.md 的 4.4、4.5 与设计文档 Correctness Properties。

package clean

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 时间标志的取值：空值与「单双周」表示每周都有，另两种只上单周或双周。
const (
	timeFlagOddEven = "单双周" // 单双周都有：等于每周
	timeFlagOdd     = "单周"  // 只上单周
	timeFlagEven    = "双周"  // 只上双周
)

// ErrWeekRange 表示周次范围或时间标志读不出周次：这些周保持未知，不当成空闲。
var ErrWeekRange = errors.New("周次范围无法解析")

// WeekRangeWeeks 把一条说明的周次范围与时间标志解析成教学周次，按从小到大返回。
// totalWeeks 是学期总周数；起点或终点落在学期外的范围读不出来，宁可保持未知也不猜它在学期内的哪几周。
func WeekRangeWeeks(weekRange, timeFlag string, totalWeeks int) ([]int, error) {
	// 没有总周数就判断不了哪些周在学期内，这时不猜
	if totalWeeks < 1 {
		return nil, fmt.Errorf("%w: 学期总周数为 %d", ErrWeekRange, totalWeeks)
	}
	weeks, err := parseWeekSegments(weekRange, totalWeeks)
	if err != nil {
		return nil, err
	}
	return filterByTimeFlag(weeks, timeFlag)
}

// parseWeekSegments 解析周次范围原文：支持「1-16」「3」以及逗号、顿号分隔的多段，每段都是闭区间。
func parseWeekSegments(raw string, totalWeeks int) ([]int, error) {
	text := strings.TrimSpace(raw)
	// 空范围说明这一格没有周次信息，对应「保持未知」
	if text == "" {
		return nil, fmt.Errorf("%w: 周次范围为空", ErrWeekRange)
	}

	weeks := make([]int, 0, totalWeeks)
	for _, segment := range strings.FieldsFunc(text, isSegmentSeparator) {
		start, end, err := parseWeekBounds(segment, totalWeeks)
		if err != nil {
			return nil, err
		}
		// 闭区间逐周展开，端点已按学期总周数校验过
		for week := start; week <= end; week++ {
			weeks = append(weeks, week)
		}
	}
	// 一段都没解析出来说明原文只有分隔符，同样按解析失败处理
	if len(weeks) == 0 {
		return nil, fmt.Errorf("%w: %q 里没有周次", ErrWeekRange, raw)
	}
	return weeks, nil
}

// parseWeekBounds 解析一段周次，返回闭区间两端：只写一个数字时它同时是起点与终点。
func parseWeekBounds(segment string, totalWeeks int) (int, int, error) {
	bounds := strings.Split(strings.TrimSpace(segment), "-")
	// 一段里最多一个连字符，多写说明这不是周次范围
	if len(bounds) > 2 {
		return 0, 0, fmt.Errorf("%w: %q 不是周次范围", ErrWeekRange, segment)
	}

	start, err := parseWeekNumber(bounds[0], totalWeeks)
	if err != nil {
		return 0, 0, err
	}
	// 只有一个数字时起点就是终点
	if len(bounds) == 1 {
		return start, start, nil
	}
	end, err := parseWeekNumber(bounds[1], totalWeeks)
	if err != nil {
		return 0, 0, err
	}
	// 起点晚于终点读不出周次，不能拿它当空范围
	if start > end {
		return 0, 0, fmt.Errorf("%w: %q 的起点晚于终点", ErrWeekRange, segment)
	}
	return start, end, nil
}

// parseWeekNumber 解析一个周次数字：必须是 1 到学期总周数之间的整数。
func parseWeekNumber(text string, totalWeeks int) (int, error) {
	trimmed := strings.TrimSpace(text)
	week, err := strconv.Atoi(trimmed)
	// 不是数字说明这一段不是周次写法
	if err != nil {
		return 0, fmt.Errorf("%w: %q 不是周次", ErrWeekRange, trimmed)
	}
	// 学期里没有这个周，整段读不出来
	if week < 1 || week > totalWeeks {
		return 0, fmt.Errorf("%w: 周次 %d 不在 1 到 %d 之间", ErrWeekRange, week, totalWeeks)
	}
	return week, nil
}

// filterByTimeFlag 按时间标志筛掉不上的周：单周留奇数周，双周留偶数周。
func filterByTimeFlag(weeks []int, timeFlag string) ([]int, error) {
	flag := normalizeLabel(timeFlag)
	// 标志为空或写「单双周」时每周都有，原样返回
	if flag == "" || flag == timeFlagOddEven {
		return weeks, nil
	}
	// 只认单周与双周两种筛法，别的写法读不出奇偶，按解析失败处理
	if flag != timeFlagOdd && flag != timeFlagEven {
		return nil, fmt.Errorf("%w: 时间标志 %q 读不出单双周", ErrWeekRange, timeFlag)
	}

	kept := make([]int, 0, len(weeks))
	for _, week := range weeks {
		// 单周只留奇数周、双周只留偶数周，其余周这一格不适用
		if (flag == timeFlagOdd && week%2 == 1) || (flag == timeFlagEven && week%2 == 0) {
			kept = append(kept, week)
		}
	}
	return kept, nil
}

// isSegmentSeparator 判断段间分隔符：周次范围可能写成半角逗号、全角逗号或顿号分隔的多段。
func isSegmentSeparator(r rune) bool {
	switch r {
	// 三种分隔符都当作段与段之间的分隔
	case ',', '，', '、':
		return true
	default:
		return false
	}
}
