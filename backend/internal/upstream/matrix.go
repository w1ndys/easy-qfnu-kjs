package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// weekMatrixPath 是按楼请求周矩阵的端点（docs/upstream-api.md 第 6 节）。
const weekMatrixPath = "/jsxsd/kbxx/jsjy_query2"

// 周矩阵表单的固定取值：按周查询，星期范围覆盖周一至周日；
// jc/jc2（节次）与 jszt（状态）一律不传，表示全时段、全状态。
const (
	matrixTypewhere    = "jszq"
	matrixWeekdayFirst = "1" // 周一
	matrixWeekdayLast  = "7" // 周日
	matrixKeywordEmpty = ""  // jsmc_mh 必须留空，楼名不能写进教室关键词
)

// 周矩阵的包内原因：空楼编号会被上游当成「不限楼」拉全校，非正周次会退化成聚合矩阵。
var (
	errEmptyTerm     = errors.New("学期编号为空")
	errEmptyBuilding = errors.New("教学楼编号为空")
	errBadWeek       = errors.New("周次必须为正整数")
)

// WeekMatrixParams 是一次周矩阵请求的参数。
type WeekMatrixParams struct {
	Term       string // 学期编号，写入 xnxqh
	BuildingID string // 白名单楼编号，写入 jxlbh；留空等于拉全校，必须拒绝
	Week       int    // 教学周，同时写入 zc 与 zc2
}

// FetchWeekMatrix 请求某栋楼某一周的周矩阵，原样返回正文交给 parser（tasklist 4.1）。
//
// 一次调用只发一次矩阵请求，因此「每楼每周一次」由调用方按楼 × 周各调一次保证；
// 请求走 doWithRelogin，与其他业务请求共用内存会话、节奏与重试。
// 参数缺失在发请求前失败：空学期编号、空楼编号与非正周次都不打上游。
func (c *Client) FetchWeekMatrix(ctx context.Context, params WeekMatrixParams) ([]byte, error) {
	if strings.TrimSpace(params.Term) == "" {
		return nil, fmt.Errorf("%w: 学期编号不能为空", errEmptyTerm)
	}
	// 空 jxlbh 会被上游当成「不限楼」，等于拉全校，不只多花请求，还会把别的楼也写进这一轮。
	if strings.TrimSpace(params.BuildingID) == "" {
		return nil, fmt.Errorf("%w: 空楼编号会被上游当成不限楼", errEmptyBuilding)
	}
	// 周次为 0 或负数会退化成全学期聚合矩阵，拿不到逐周状态。
	if params.Week < 1 {
		return nil, fmt.Errorf("%w: 实际 %d", errBadWeek, params.Week)
	}

	resp, err := c.doWithRelogin(ctx, request{
		method: http.MethodPost,
		path:   weekMatrixPath,
		form:   weekMatrixForm(params),
	})
	if err != nil {
		return nil, fmt.Errorf("读取第 %d 周矩阵: %w", params.Week, err)
	}
	return resp.Body, nil
}

// weekMatrixForm 按 docs/upstream-api.md 第 6 节拼表单：楼编号进 jxlbh，周次同时写 zc 与 zc2。
func weekMatrixForm(params WeekMatrixParams) url.Values {
	week := strconv.Itoa(params.Week)
	return url.Values{
		"typewhere": {matrixTypewhere},
		"xnxqh":     {params.Term},
		"jxlbh":     {params.BuildingID},
		"jsmc_mh":   {matrixKeywordEmpty},
		"zc":        {week},
		"zc2":       {week},
		"xq":        {matrixWeekdayFirst},
		"xq2":       {matrixWeekdayLast},
	}
}
