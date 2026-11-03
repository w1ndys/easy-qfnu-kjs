package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	// buildingListPath 是教学楼下拉列表的 AJAX 端点（docs/upstream-api.md 第 5 节）。
	buildingListPath = "/jsxsd/kbxx/jsjy_processAjax"
	// buildingListRequestType 固定取教学楼下拉；jxq/newjs/gnq 是别的下拉，不能混用。
	buildingListRequestType = "jxl"
)

// CampusQufu 是曲阜校区的 xqid；取值来自 docs/upstream-api.md 第 5 节的实测记录。
const CampusQufu = "1"

// CampusRizhao 是日照校区的 xqid；接口本身不限制只有这两个校区。
const CampusRizhao = "2"

// 楼列表的包内原因：参数缺失在发请求前失败，响应非法属于数据级失败。
var (
	errEmptyCampus         = errors.New("校区 id 为空")
	errBuildingListInvalid = errors.New("楼列表响应不是有效的 JSON 数组")
)

// Building 是教学楼列表的一项：ID 是上游 dm，也是矩阵与白名单用的楼编号 jxlbh；
// Name 是上游 dmmc，只作展示，采集不把它写进 jsmc_mh。
type Building struct {
	ID   string // 楼编号 jxlbh
	Name string // 展示名 dmmc
}

// FetchBuildings 按校区读取教学楼下拉列表（tasklist 3.4）。
//
// 请求走 doWithRelogin，与其他业务请求共用同一个内存会话、节奏与重试；
// campusID 写入 xqid，requestType 固定为 jxl。返回项原样映射 dm/dmmc：
// 文档与旧实现都没有规定要过滤空编号或空名称，本方法不做过滤，由调用方校验白名单。
func (c *Client) FetchBuildings(ctx context.Context, campusID string) ([]Building, error) {
	// 空 xqid 会让上游拿不到校区，等于没有限定范围，必须在发请求前挡掉。
	if strings.TrimSpace(campusID) == "" {
		return nil, fmt.Errorf("%w: 校区 id 不能为空", errEmptyCampus)
	}

	resp, err := c.doWithRelogin(ctx, request{
		method: http.MethodGet,
		path:   buildingListPath,
		query: url.Values{
			"xqid":        {campusID},
			"requestType": {buildingListRequestType},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("读取教学楼列表: %w", err)
	}
	return parseBuildingList(resp.Body)
}

// parseBuildingList 把 JSON 数组映射成 Building 列表：dm → ID，dmmc → Name。
// 空数组与 JSON null 都视为「本校区没有楼」，返回空列表而不是错误；
// 非数组、语法错误或空正文返回 errBuildingListInvalid。
func parseBuildingList(body []byte) ([]Building, error) {
	var items []struct {
		DM   string `json:"dm"`
		DMMC string `json:"dmmc"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("%w: %w: 响应 %d 字节", errBuildingListInvalid, err, len(body))
	}

	// 始终返回非 nil 切片，调用方序列化成 JSON 时得到 [] 而不是 null。
	buildings := make([]Building, 0, len(items))
	for _, item := range items {
		buildings = append(buildings, Building{ID: item.DM, Name: item.DMMC})
	}
	return buildings, nil
}
