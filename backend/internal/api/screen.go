package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// screenInfo 返回数据大屏聚合：规模、版本、逐周新鲜度、最近轮次、异常计数与占用分布。
// 无当前版本时查询层返回 release:null + empty:true，这里照常回 200（Requirement 33.8）。
// 大屏不记访问统计：它是每 30 秒一次的轮询，记进去会把 PV 变成在线时长。
func (s *Server) screenInfo(w http.ResponseWriter, r *http.Request) {
	// 装配缺项属于部署缺陷：给通用内部错误，不在公开响应里暴露细节。
	if s.screen == nil {
		s.log.Error("大屏查询未装配")
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}

	// 参数错误与 /api/query 共用同一口径：HTTP 400 + 业务码 40001，错误文案只描述用户可见问题。
	params, err := parseScreenParams(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, 40001, err.Error())
		return
	}

	result, err := s.screen.Screen(r.Context(), s.currentTime(), params)
	// 与其它公开端点一致：客户端断开不写响应、不打 ERROR。
	if errors.Is(err, context.Canceled) {
		s.log.Info("客户端已断开，放弃本次响应")
		return
	}
	// 失败只给业务码与通用文案：大屏响应里出现错误码或错误原文都算违约（Requirement 33.6、CP13）。
	if err != nil {
		s.log.Error("大屏查询失败", "err", err)
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}
	s.ok(w, result)
}

// parseScreenParams 解析大屏的 peak_days：热力、峰值与 Top 10 的窗口只接受 7 与 30，缺省近 7 日。
// 取值语义由 stats 侧定义，这里只做接口层的枚举校验，非法值上抛给 handler 映射成 40001。
func parseScreenParams(r *http.Request) (query.ScreenParams, error) {
	days, err := parseOptionalInt(r.URL.Query().Get("peak_days"), stats.DefaultPeakDays)
	if err != nil {
		return query.ScreenParams{}, fmt.Errorf("peak_days 必须是整数")
	}
	if days != stats.PeakDays7 && days != stats.PeakDays30 {
		return query.ScreenParams{}, fmt.Errorf("peak_days 取值 7 或 30")
	}
	return query.ScreenParams{PeakDays: days}, nil
}
