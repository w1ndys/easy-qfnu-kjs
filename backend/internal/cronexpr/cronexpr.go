// Package cronexpr 解析标准 5 字段 cron 表达式（分 时 日 月 周），并按业务时区
// 求值。它是一组纯函数：不读库、不发请求、不取时钟，`Next` 的搜索起点由调用方
// 注入，所以同一表达式在同一时刻的结果恒定，可以直接做表驱动与属性测试。
//
// 语法只覆盖调度真正需要的部分：每个字段接受 `*`、`*/n`、`a`、`a-b`、`a-b/n`
// 以及它们的逗号列表。秒级字段、`@every` 这类别名、`JAN`/`MON` 这类名称一律
// 以 ErrInvalid 拒绝，不做「宽容解析」——校验与调度共用这个 Parse，多接受的写法
// 等于多出一批「校验通过但排不了」的表达式（Requirement 5.1 / 5.3、CP8）。
//
// 依赖方向见 specs/2026-11-08-admin-settings-sync/design.md：本包只 import
// 标准库与 model，业务时区只有 model.BusinessLocation 一个来源。
package cronexpr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrInvalid 是表达式非法的哨兵。Parse 返回的每个错误都用 %w 包住它，
// 调用方既能 errors.Is(err, ErrInvalid) 只判类别，也能读错误文案取字段位置
// （scheduler.Reload 与 settings.Validate 都是这么用的）。
var ErrInvalid = errors.New("cron 表达式非法")

const (
	// fieldCount 是表达式的字段个数：分 时 日 月 周。字段多于或少于这个数都
	// 直接拒绝，不猜「是不是把秒写进来了」。
	fieldCount = 5

	// nextSearchYears 是 Next 的搜索上限。超过这个时长还没有命中就返回 false，
	// 例如 `0 0 30 2 *`（2 月没有 30 日）永远不可达，不应该让调用方无限等下去。
	nextSearchYears = 5
)

// fieldSpec 描述一个字段的名字与取值区间。名字进错误文案，让管理员看到的是
// 「第 3 个字段（日）取值 32 超出 1–31」而不是「parse error」。
type fieldSpec struct {
	pos     int    // 字段位置，从 1 开始，与错误文案里的「第几个」一致
	name    string // 字段名，与错误文案里的括号内容一致
	min     int    // 闭区间下界
	max     int    // 闭区间上界
	weekday bool   // 周字段：取值 7 要归一成 0（周日），其余字段无此归一
}

// fieldSpecs 的顺序就是表达式的字段顺序，不要重排：错误文案里的位置靠它，
// Next 的「月 → 日 → 时 → 分」跳进也按这个顺序逐级收敛。
var fieldSpecs = [fieldCount]fieldSpec{
	{pos: 1, name: "分钟", min: 0, max: 59},
	{pos: 2, name: "小时", min: 0, max: 23},
	{pos: 3, name: "日", min: 1, max: 31},
	{pos: 4, name: "月", min: 1, max: 12},
	{pos: 5, name: "周", min: 0, max: 7, weekday: true},
}

// Schedule 是解析后的 5 字段表达式。每个字段是一张位图（第 i 位代表取值 i），
// 匹配一次只做几次位运算，不逐项比对字符串。
//
// domStar / dowStar 只记录该字段是否**恰为** `*`：日与周都不恰为 `*` 时按 OR
// 触发，恰有一个为 `*` 时按另一个匹配（Requirement 5.7）。`*/n` 即使 n=1 也记为
// 受限，与 Vixie cron 的「只有字面量 `*` 才算不限」保持一致——把 `*/2` 当 `*`
// 会让「每月 1 日与每周一」这类表达式悄悄变成「每天都触发」。
type Schedule struct {
	minute, hour, dom, month, dow uint64 // 五张位图，取值域见 fieldSpecs
	domStar, dowStar              bool   // 日/周字段是否恰为 `*`
	expr                          string // 管理员原写法，仅供日志与 String()
}

// Parse 解析 5 字段表达式（分 时 日 月 周）。字段只接受 `*`、`*/n`、`a`、`a-b`、
// `a-b/n` 与逗号列表；取值域是 0–59、0–23、1–31、1–12、0–7。周字段的 7 归一成
// 周日（0），与 `0` 等价。
//
// 拒绝的写法包括：字段数不是 5、`*/0` 或非正步长、倒序区间、空列表项（含
// `1,,2` 与结尾逗号）、`a/n` 这种「单值带步长」、字母名称与 `@every` 别名、
// 空串、前后多余空白。错误文案一律带第几个字段与字段名，供 42201 直接透出。
func Parse(expr string) (Schedule, error) {
	if strings.TrimSpace(expr) == "" {
		return Schedule{}, fmt.Errorf("%w：表达式为空", ErrInvalid)
	}

	// 前后空白与多空格不静默修掉：表达式存进库后还要回显给管理员，
	// 悄悄 trim 会让「存进去的」和「显示出来的」不是同一串字符。
	// 用单个空格切分，连续空格就会切出空字段，由下面的字段解析报位置。
	if expr != strings.TrimSpace(expr) {
		return Schedule{}, fmt.Errorf("%w：表达式前后不得有多余空白", ErrInvalid)
	}

	parts := strings.Split(expr, " ")
	if len(parts) != fieldCount {
		return Schedule{}, fmt.Errorf("%w：需要 %d 个字段（分 时 日 月 周），实际 %d 个",
			ErrInvalid, fieldCount, len(parts))
	}

	var (
		sched Schedule
		err   error
	)
	sched.expr = expr

	if sched.minute, _, err = parseField(fieldSpecs[0], parts[0]); err != nil {
		return Schedule{}, err
	}
	if sched.hour, _, err = parseField(fieldSpecs[1], parts[1]); err != nil {
		return Schedule{}, err
	}
	if sched.dom, sched.domStar, err = parseField(fieldSpecs[2], parts[2]); err != nil {
		return Schedule{}, err
	}
	if sched.month, _, err = parseField(fieldSpecs[3], parts[3]); err != nil {
		return Schedule{}, err
	}
	if sched.dow, sched.dowStar, err = parseField(fieldSpecs[4], parts[4]); err != nil {
		return Schedule{}, err
	}

	return sched, nil
}

// Startup 是一次启动期表达式的求值结果。回落规则只有这一份实现（ResolveStartup）：
// 装配层打启动日志、调度器装启动计划都调它，两边才不会各判一次，把「日志里的值」
// 与「进程真正调度的值」写成两个答案。
//
// 空串与非法值是两种口径：空串表示「库里没有这一行」，属于正常状态，按缺失处理、
// 不记错误（Requirement 9.5）；非法值表示存量被写坏，要记 ERROR（Requirement 9.6）。
// 两者都回落 model.DefaultCronExpr，但由 Missing 与 Invalid 各自标记，调用方按标记
// 选日志级别，不再拿 Raw 反推「这算不算缺失」——那样等于把分类规则抄到每个调用点。
type Startup struct {
	Raw     string // 来源给出的原样值，供日志点名
	Expr    string // 生效表达式的原写法：合法值原样，其余是 model.DefaultCronExpr
	Missing bool   // 来源没有给出值（空串或纯空白），属于正常状态而不是错误
	Invalid error  // 原值被拒的原因；缺失与合法值都是 nil
}

// ResolveStartup 按回落规则算出启动期真正生效的表达式。
//
// 它不校验回落后的 Expr：model.DefaultCronExpr 被改坏是代码问题而不是管理员输入，
// 由调用方 Parse(Expr) 时立刻失败并决定「暂停调度」。选值与解码分开，装配层才可能
// 只为了打一条启动日志而复用同一份规则，而不是自己再写一遍 Parse + 回落。
//
// 三种结局只在这里分类一次：缺失（Missing）、非法（Invalid）、合法（两者皆无）。
// 「是否回落到默认值」不再单独存一份字段，它等价于 Expr != Raw，存两份迟早写岔。
func ResolveStartup(raw string) Startup {
	// 空串与纯空白都按缺失处理：settings.Service 缺行时已经回 DefaultCronExpr，
	// 这里再判一次是给别的 ExprSource 实现兜底，口径不靠调用方自觉。
	if strings.TrimSpace(raw) == "" {
		return Startup{Raw: raw, Expr: model.DefaultCronExpr, Missing: true}
	}

	if _, err := Parse(raw); err != nil {
		return Startup{Raw: raw, Expr: model.DefaultCronExpr, Invalid: err}
	}

	return Startup{Raw: raw, Expr: raw}
}

// parseField 把一个字段解析成位图，并报告它是否恰为 `*`。
// 列表里的每一项独立解析，位图取并集；空项（`1,,2`、`1,`、`,1`）直接拒绝。
// 列表项也可以写 `*` 或 `*/n`（如 `1,*`），但整个字段不恰为 `*`，
// 因此仍按受限处理——日/周字段的 OR 判定只认整字段是一个孤零零的 `*`。
func parseField(spec fieldSpec, text string) (uint64, bool, error) {
	if text == "*" {
		return fullRange(spec), true, nil
	}

	var bits uint64
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, false, fmt.Errorf("%w：第 %d 个字段（%s）列表里有空项",
				ErrInvalid, spec.pos, spec.name)
		}

		itemBits, err := parseItem(spec, item)
		if err != nil {
			return 0, false, err
		}
		bits |= itemBits
	}

	return bits, false, nil
}

// parseItem 解析列表里的一项：`*`、`*/n`、`a`、`a-b`、`a-b/n`。
// 单值带步长（`a/n`）不在允许的写法里，直接拒绝而不是猜成 `a-b/n`。
func parseItem(spec fieldSpec, item string) (uint64, error) {
	base, stepText, hasStep := strings.Cut(item, "/")

	if base == "*" {
		if !hasStep {
			return fullRange(spec), nil
		}

		step, err := parseStep(spec, stepText)
		if err != nil {
			return 0, err
		}

		return steppedRange(spec, spec.min, spec.max, step), nil
	}

	loText, hiText, hasRange := strings.Cut(base, "-")
	if !hasRange {
		if hasStep {
			return 0, fieldErrorf(spec, "不支持 %q：步长只能跟在 `*` 或 `a-b` 之后", item)
		}

		value, err := parseValue(spec, base)
		if err != nil {
			return 0, err
		}

		return bitFor(spec, value), nil
	}

	lo, err := parseValue(spec, loText)
	if err != nil {
		return 0, err
	}
	hi, err := parseValue(spec, hiText)
	if err != nil {
		return 0, err
	}
	if lo > hi {
		return 0, fieldErrorf(spec, "区间 %q 起点大于终点", item)
	}

	step := 1
	if hasStep {
		if step, err = parseStep(spec, stepText); err != nil {
			return 0, err
		}
	}

	return steppedRange(spec, lo, hi, step), nil
}

// parseStep 解析步长。取值与步长共用 digitsOnly 这道判定，所以 `*/+5`、
// `1-2/+1` 和 `+5`、`1-+5` 一样被拒：Atoi 会接受前导符号，但那不是 cron 的写法。
// 0 与负数同样拒绝：`*/0` 没有「每 0 分钟」这种语义，放行只会让调用方在 Next
// 里原地打转。
func parseStep(spec fieldSpec, text string) (int, error) {
	if !digitsOnly(text) {
		return 0, fieldErrorf(spec, "步长 %q 不是数字", text)
	}

	step, err := strconv.Atoi(text)
	if err != nil {
		return 0, fieldErrorf(spec, "步长 %q 超出范围", text)
	}
	if step < 1 {
		return 0, fieldErrorf(spec, "步长 %d 必须 ≥ 1", step)
	}

	return step, nil
}

// parseValue 解析单个取值并做区间校验。这里用 Atoi 之前先要求「只有数字」：
// Atoi 会接受 `+5` 这样的符号，而带符号的取值不是 cron 的写法；空的取值
// （`-1`、`1-` 会切出空段）也要报得比「不是数字」更直白。
func parseValue(spec fieldSpec, text string) (int, error) {
	if text == "" {
		return 0, fieldErrorf(spec, "缺少取值")
	}

	if !digitsOnly(text) {
		return 0, fieldErrorf(spec, "%q 不是数字（不接受字母名称与 @ 别名）", text)
	}

	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fieldErrorf(spec, "取值 %q 超出范围", text)
	}
	if value < spec.min || value > spec.max {
		return 0, fieldErrorf(spec, "取值 %d 超出 %d–%d", value, spec.min, spec.max)
	}

	return value, nil
}

// digitsOnly 报告 text 是否只由 ASCII 数字组成（空串返回 false）。取值与步长
// 都在 Atoi 之前过这一关，符号与字母一律不放过，两处口径因此不会走岔。
func digitsOnly(text string) bool {
	if text == "" {
		return false
	}

	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// fullRange 返回字段整个取值域的位图。
func fullRange(spec fieldSpec) uint64 {
	return steppedRange(spec, spec.min, spec.max, 1)
}

// steppedRange 返回 [lo, hi] 里从 lo 起每隔 step 取一个的位图。
func steppedRange(spec fieldSpec, lo, hi, step int) uint64 {
	var bits uint64
	for value := lo; value <= hi; value += step {
		bits |= bitFor(spec, value)
	}

	return bits
}

// bitFor 返回单个取值对应的位，周字段顺便把 7 归一成 0（周日）。
func bitFor(spec fieldSpec, value int) uint64 {
	if spec.weekday && value == 7 {
		value = 0
	}

	return uint64(1) << uint(value)
}

// fieldErrorf 拼字段级错误：哨兵 + 第几个 + 字段名 + 具体原因。
// 所有文案都从这一处出口生成，位置与名字不会各写一份而对不上。
func fieldErrorf(spec fieldSpec, format string, args ...any) error {
	return fmt.Errorf("%w：第 %d 个字段（%s）%s",
		ErrInvalid, spec.pos, spec.name, fmt.Sprintf(format, args...))
}

// Matches 报告时刻是否命中。入参先换算到业务时区（+08:00），再按分钟粒度匹配：
// 同一分钟内的秒与纳秒不影响结果，因为调度本来就是按分钟触发的。
// 零值 Schedule（没经过 Parse）不命中任何时刻。
func (s Schedule) Matches(t time.Time) bool {
	local := t.In(model.BusinessLocation())

	if s.minute&(uint64(1)<<uint(local.Minute())) == 0 {
		return false
	}
	if s.hour&(uint64(1)<<uint(local.Hour())) == 0 {
		return false
	}
	if s.month&(uint64(1)<<uint(local.Month())) == 0 {
		return false
	}

	return s.matchesDay(local)
}

// matchesDay 实现日与周的触发口径（Requirement 5.7）：
//   - 两者都恰为 `*`：每天都触发，不再看位图（位图此时也是满的，但语义写死在这里）；
//   - 只有日受限：按日匹配；
//   - 只有周受限：按周匹配；
//   - 两者都受限：任一命中即触发（OR，不是 AND）。
//
// 最后一条最容易写错成 AND，那会让「每月 1 日与每周一」变成「每月 1 日且恰为周一」，
// 一年只响一两次。
func (s Schedule) matchesDay(t time.Time) bool {
	domHit := s.dom&(uint64(1)<<uint(t.Day())) != 0
	dowHit := s.dow&(uint64(1)<<uint(t.Weekday())) != 0

	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowHit
	case s.dowStar:
		return domHit
	default:
		return domHit || dowHit
	}
}

// Next 返回严格晚于 after 的第一个命中时刻，时刻落在业务时区、秒与纳秒为 0。
// 搜索上限 nextSearchYears 年，超时返回 false（例如 2 月 30 日永远不可达）。
//
// 实现按「月 → 日 → 时 → 分」逐级跳，不逐分钟穷举：字段不命中时直接跳到该字段的
// 下一个边界（下个月 1 日 0 点、次日 0 点、下一个整点），所以一次搜索最多是
// 「月数 + 每月天数 + 小时数 + 分钟数」量级，而不是 5 年的分钟数。
//
// 严格晚于入参先从「截到分钟再进一分钟」做起：入参落在命中分钟的 30 秒处时，
// 不能把这一分钟再触发一次（Requirement 5.5 的分钟粒度、同一分钟不重复触发）。
func (s Schedule) Next(after time.Time) (time.Time, bool) {
	loc := model.BusinessLocation()

	candidate := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(nextSearchYears, 0, 0)

	for candidate.Before(limit) {
		if s.month&(uint64(1)<<uint(candidate.Month())) == 0 {
			// 整月不命中：跳到下个月 1 日 0 点，不逐日空转。
			candidate = time.Date(candidate.Year(), candidate.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}

		if !s.matchesDay(candidate) {
			// 当天不命中：跳到次日 0 点；跨月由下一轮重新判月份。
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}

		if s.hour&(uint64(1)<<uint(candidate.Hour())) == 0 {
			// 本小时不命中：跳到下一个整点；跨日由下一轮重新判日与月份。
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(),
				candidate.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}

		if s.minute&(uint64(1)<<uint(candidate.Minute())) == 0 {
			candidate = candidate.Add(time.Minute)
			continue
		}

		return candidate, true
	}

	return time.Time{}, false
}

// String 返回管理员原写法（已通过 Parse 的空白校验），只供启动日志与测试断言。
// 不做「规范化成每字段最小写法」：日志要与库里存的值逐字对得上，排查时才知道
// 管理员提交的是哪一串。
func (s Schedule) String() string {
	return s.expr
}
