package api

import (
	"context"
	"errors"
	"net/http"
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

	result, err := s.screen.Screen(r.Context(), s.currentTime())
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
