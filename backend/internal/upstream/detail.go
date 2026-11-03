package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// occupancyDetailPath 是占用说明（明细）的下钻端点（docs/upstream-api.md 第 8 节）。
const occupancyDetailPath = "/jsxsd/kbxx/jsjy_jszyqk"

// 占用说明查询的固定取值：按周查询、以 add 方式下钻；
// 起止周次、起止节次、起止时间与教室状态一律留空，星期范围覆盖周一至周日，
// 真正定位格子的只有 xq 与 kcsj（docs/upstream-api.md 第 8 节）。
const (
	detailTypewhere = "jszq"
	detailType      = "add"
	detailStartXq   = "1" // 周一
	detailEndXq     = "7" // 周日
)

// detailBigSlots 是 5 个大节在矩阵表头里的 tdvalue，也是 kcsj 的后半段：
// 0102 → 01、02，030405 → 03、04、05，0607、0809、101112 以此类推。
// kcsj 由「星期 + 这里的代码」拼成，例如周一第一大节是 10102。
var detailBigSlots = []string{"0102", "030405", "0607", "0809", "101112"}

// 占用说明的包内原因：参数缺失或非法都在发请求前失败，不打上游。
var (
	errKBJCMSIDEmpty = errors.New("节次模式 id 为空")
	errRoomIDEmpty   = errors.New("房号为空")
	errBadWeekday    = errors.New("星期必须落在 1 到 7 之间")
	errBadBigSlot    = errors.New("大节代码不在五个大节之内")
)

// OccupancyDetailParams 是一次占用说明请求的参数。
type OccupancyDetailParams struct {
	Term     string // 学期编号，写入 xnxqh
	KBJCMSID string // 节次模式 id，来自学期页（FetchTerm），每次登录可能不同，不能写死
	RoomID   string // 房号 jsbh，写入 jsbh；它是教室的唯一身份
	Weekday  int    // 星期，1=周一 … 7=周日；写入 xq，也是 kcsj 的第一位
	BigSlot  string // 大节代码，写入 kcsj 的后半段，只能取 detailBigSlots 里的五个值
}

// FetchOccupancyDetail 请求某个格子（房号 × 星期 × 大节）的占用说明，
// 原样返回正文交给 parser（tasklist 4.2）提取四项字段。
//
// 一次调用只下一次钻，所以「同一格至多一条任务」由调用方按格各调一次保证；
// 请求走 doWithRelogin，与其他业务请求共用内存会话、节奏与重试。
// 参数缺失或非法在发请求前失败：空学期编号、空 kbjcmsid、空房号、星期越界与
// 大节代码不在五块之内都不打上游，避免用错参数换来一个看似正常的错误页。
func (c *Client) FetchOccupancyDetail(ctx context.Context, params OccupancyDetailParams) ([]byte, error) {
	if strings.TrimSpace(params.Term) == "" {
		return nil, fmt.Errorf("%w: 学期编号不能为空", errEmptyTerm)
	}
	// kbjcmsid 每次登录/学期都可能不同；空值会让明细页拿不到节次模式。
	if strings.TrimSpace(params.KBJCMSID) == "" {
		return nil, fmt.Errorf("%w: 节次模式 id 不能为空", errKBJCMSIDEmpty)
	}
	// 空 jsbh 会被上游当成「不限教室」，等于把整栋楼的明细都拉回来。
	if strings.TrimSpace(params.RoomID) == "" {
		return nil, fmt.Errorf("%w: 空房号会被上游当成不限教室", errRoomIDEmpty)
	}
	if params.Weekday < 1 || params.Weekday > 7 {
		return nil, fmt.Errorf("%w: 实际 %d", errBadWeekday, params.Weekday)
	}
	if !slices.Contains(detailBigSlots, params.BigSlot) {
		return nil, fmt.Errorf("%w: 实际 %q", errBadBigSlot, params.BigSlot)
	}

	resp, err := c.doWithRelogin(ctx, request{
		method: http.MethodGet,
		path:   occupancyDetailPath,
		query:  occupancyDetailQuery(params),
	})
	if err != nil {
		return nil, fmt.Errorf("读取房号 %s 星期 %d 大节 %s 的占用说明: %w",
			params.RoomID, params.Weekday, params.BigSlot, err)
	}
	return resp.Body, nil
}

// occupancyDetailQuery 按 docs/upstream-api.md 第 8 节拼查询串：
// kcsj 是「星期 + 大节代码」，xq 与 kcsj 的第一位一致；
// 起止周次/节次/时间与 jszt 显式留空，不是不传，保持与文档记录的请求形状一致。
func occupancyDetailQuery(params OccupancyDetailParams) url.Values {
	weekday := strconv.Itoa(params.Weekday)
	return url.Values{
		"xnxqh":     {params.Term},
		"jsbh":      {params.RoomID},
		"kcsj":      {weekday + params.BigSlot},
		"xq":        {weekday},
		"typewhere": {detailTypewhere},
		"startZc":   {""},
		"endZc":     {""},
		"startJc":   {""},
		"endJc":     {""},
		"startXq":   {detailStartXq},
		"endXq":     {detailEndXq},
		"kssj":      {""},
		"jssj":      {""},
		"jszt":      {""},
		"type":      {detailType},
		"kbjcmsid":  {params.KBJCMSID},
	}
}
