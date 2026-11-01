package api

import (
	"context"
	"errors"
	"net/http"
)

// metaInfo 返回版本标识、生成时间、字典、轴、白名单教学楼与逐周新鲜度。
// 无当前版本时查询层返回 release:null + empty:true，这里照常回 200。
func (s *Server) metaInfo(w http.ResponseWriter, r *http.Request) {
	// 装配缺项属于部署缺陷：给通用内部错误，不在公开响应里暴露细节。
	if s.meta == nil {
		s.log.Error("元信息查询未装配")
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}

	result, err := s.meta.Meta(r.Context(), s.currentTime())
	// 与其它公开端点一致：客户端断开不写响应、不打 ERROR。
	if errors.Is(err, context.Canceled) {
		s.log.Info("客户端已断开，放弃本次响应")
		return
	}
	if err != nil {
		s.log.Error("元信息查询失败", "err", err)
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}
	s.ok(w, result)
}
