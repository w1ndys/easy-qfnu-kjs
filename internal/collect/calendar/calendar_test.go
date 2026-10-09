package calendar

// 本文件是 calendar 的单元测试：不访问网络、不写库、不涉及 Cookie，
// 正文都是手写的最小样本。固定日期表依据 specs/collector-full-sync/design.md 的周历公式、
// 任务 2.1 的用例与 docs/decisions/2026-10-09-term-week-from-one-sample.md 的实测文本。

import (
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestBuildWeeksFixedDates 用固定日期表校验周一推算：采样日期、星期几与周次决定整个周历。
func TestBuildWeeksFixedDates(t *testing.T) {
	cases := []struct {
		name        string // 用例说明
		sampleDate  string // 采样日期（YYYY-MM-DD）
		week        int    // 「第 X 周」
		total       int    // 「共 Y 周」
		firstMonday string // 期望的第 1 周周一
	}{
		// 任务 2.1 指定用例：2026-10-30 是星期五，第 6 周，共 16 周
		{"周五采样第 6 周", "2026-10-30", 6, 16, "2026-09-21"},
		// 换一个采样日（跨年后的周一）必须推出同一个第 1 周周一
		{"周一采样第 16 周", "2027-01-04", 16, 16, "2026-09-21"},
		// 周日采样：星期几为 7，所在周的周一是它前面 6 天
		{"周日采样第 1 周", "2026-09-27", 1, 16, "2026-09-21"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sampleDate := mustParseDate(t, testCase.sampleDate)
			sample := Sample{Week: testCase.week, Total: testCase.total}

			weeks, err := BuildWeeks("2026-2027-1", sampleDate, sample)
			if err != nil {
				t.Fatalf("生成周历失败: %v", err)
			}
			// 行数必须等于总周数，少一行就是半截周历
			if len(weeks) != testCase.total {
				t.Fatalf("生成 %d 行，期望 %d 行", len(weeks), testCase.total)
			}

			got := weeks[0].Monday.Format("2006-01-02")
			if got != testCase.firstMonday {
				t.Errorf("第 1 周周一 = %s，期望 %s", got, testCase.firstMonday)
			}
			checkContiguousWeeks(t, weeks)

			// 采样日必须落在它自己那一周内，否则周次反推出来的锚点就是错的
			anchor := weeks[testCase.week-1].Monday
			if sampleDate.Before(anchor) || sampleDate.After(anchor.AddDate(0, 0, 6)) {
				t.Errorf("采样日 %s 不在第 %d 周（周一 %s）内", testCase.sampleDate, testCase.week, anchor.Format("2006-01-02"))
			}
		})
	}
}

// TestBuildWeeksRejectsBadSample 断言采样范围不合法时不生成任何周历行。
func TestBuildWeeksRejectsBadSample(t *testing.T) {
	sampleDate := mustParseDate(t, "2026-10-30")

	cases := []struct {
		name   string // 用例说明
		sample Sample // 采样结果
	}{
		{"周次大于总周数", Sample{Week: 17, Total: 16}},
		{"周次为零", Sample{Week: 0, Total: 16}},
		{"总周数为零", Sample{Week: 1, Total: 0}},
		{"周次为负", Sample{Week: -1, Total: 16}},
	}

	for _, testCase := range cases {
		weeks, err := BuildWeeks("2026-2027-1", sampleDate, testCase.sample)
		// 这种采样推不出可靠的学期起点，必须失败
		if !errors.Is(err, ErrBadWeekRange) {
			t.Errorf("%s 返回错误 %v，期望 ErrBadWeekRange", testCase.name, err)
		}
		// 失败时不能产出半截周历给调用方写入
		if len(weeks) != 0 {
			t.Errorf("%s 仍然生成了 %d 行，期望 0 行", testCase.name, len(weeks))
		}
	}
}

// TestParseSample 断言只认周次那一句：实测写法与契约写法都要认，缺文本与越界都要失败。
func TestParseSample(t *testing.T) {
	cases := []struct {
		name    string // 用例说明
		body    string // 正文内容
		want    Sample // 期望的采样结果
		wantErr error  // 期望的错误；nil 表示应当解析成功
	}{
		{"实测写法", "第6周/16周", Sample{Week: 6, Total: 16}, nil},
		{"契约写法带空格与共字", "第 6 周 / 共 16 周", Sample{Week: 6, Total: 16}, nil},
		{"夹在其它文本里", "当前学期 2026-2027-1，第6周/16周，其余内容不解析", Sample{Week: 6, Total: 16}, nil},
		{"非教学周没有这句", "不在教学周历内", Sample{}, ErrNoWeekText},
		{"空正文", "", Sample{}, ErrNoWeekText},
		{"周次为零", "第0周/16周", Sample{}, ErrBadWeekRange},
		{"周次大于总周数", "第17周/16周", Sample{}, ErrBadWeekRange},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sample, err := ParseSample([]byte(testCase.body))

			// 失败用例：错误类型要对上，采样值不能当成有效结果往外传
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("返回错误 %v，期望 %v", err, testCase.wantErr)
				}
				if sample != (Sample{}) {
					t.Errorf("失败时返回采样 %+v，期望零值", sample)
				}
				return
			}

			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if sample != testCase.want {
				t.Errorf("采样 = %+v，期望 %+v", sample, testCase.want)
			}
		})
	}
}

// mustParseDate 解析测试用的采样日期，格式不对直接失败。
func mustParseDate(t *testing.T, value string) time.Time {
	t.Helper()

	date, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatalf("解析测试日期 %s 失败: %v", value, err)
	}
	return date
}

// checkContiguousWeeks 校验周次从 1 连续排到总周数、每行都是教学周、相邻两周正好差 7 天。
func checkContiguousWeeks(t *testing.T, weeks []model.TermWeek) {
	t.Helper()

	for index, week := range weeks {
		// 周次必须从 1 连续排到总周数，中间缺号会让查询整周找不到覆盖
		if week.Week != index+1 {
			t.Errorf("第 %d 行的周次 = %d，期望 %d", index+1, week.Week, index+1)
		}
		// 采样生成的行都是教学周，否则查询侧会把这一周当假期跳过
		if !week.InCalendar {
			t.Errorf("第 %d 周 in_calendar = false，期望 true", week.Week)
		}
		// 相邻两周相差 7 天：教学周按周一到周日连续处理
		if week.Week > 1 && week.Monday.Sub(weeks[index-1].Monday) != 7*24*time.Hour {
			t.Errorf("第 %d 周与上一周相差 %v，期望 7 天", week.Week, week.Monday.Sub(weeks[index-1].Monday))
		}
	}
}

// TestParseTermReadsTermNumber 断言学期页里只取 YYYY-YYYY-N 这一个编号（需求 1.1）。
// 样本按 docs/upstream.md 第 2 节记录的格式手写，真实学期页不进 git。
func TestParseTermReadsTermNumber(t *testing.T) {
	cases := []struct {
		name string // 用例说明
		body string // 学期页正文片段
		want string // 期望取到的学期编号
	}{
		{"选项里的学期编号", `当前学期：2026-2027-1`, "2026-2027-1"},
		{"值属性里的学期编号", `value="2025-2026-3"`, "2025-2026-3"},
		{"两位数的第几学期", `2026-2027-10`, "2026-2027-10"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseTerm([]byte(testCase.body))
			if err != nil {
				t.Fatalf("解析学期编号失败: %v", err)
			}
			if got != testCase.want {
				t.Errorf("学期编号 = %q，期望 %q", got, testCase.want)
			}
		})
	}
}

// TestParseTermWithoutTerm 断言取不到学期编号时失败：宁可整轮停下，也不猜一个学期去请求。
func TestParseTermWithoutTerm(t *testing.T) {
	cases := []struct {
		name string // 用例说明
		body string // 学期页正文片段
	}{
		{"空正文", ""},
		{"登录页", `用户登录`},
		{"只有年份区间没有学期序号", `2026-2027`},
		{"学期序号不是数字", `2026-2027-第一学期`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 认不出学期编号时必须失败，调用方据此把本轮标成失败
			if _, err := ParseTerm([]byte(testCase.body)); !errors.Is(err, ErrNoTerm) {
				t.Fatalf("返回 %v，期望 ErrNoTerm", err)
			}
		})
	}
}
