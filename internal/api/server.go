// 本文件属于 entry 层：三个只读查询端点的 HTTP 入口。
// 路由、参数取值与错误码映射都以 docs/contract/api.v2.md 为准。

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// Querier 是本层需要的查询能力，测试用假实现替换。
type Querier interface {
	// Context 返回今天落在哪个学期、哪一周、星期几
	Context(ctx context.Context) (model.CalendarContext, error)
	// Meta 返回数据版本、字典、节次轴与逐周新鲜度
	Meta(ctx context.Context) (query.MetaResult, error)
	// Query 按视图返回空教室或全天状态
	Query(ctx context.Context, params query.Params) (query.QueryResult, error)
}

// Server 承载三个只读端点。
type Server struct {
	querier Querier        // 查询能力，测试用假实现替换
	loc     *time.Location // 时间字段输出用的时区，固定 Asia/Shanghai
	logger  *slog.Logger   // 结构化日志
}

// NewServer 组装 HTTP 处理器；loc 用于把时间字段按 Asia/Shanghai 输出。
func NewServer(querier Querier, loc *time.Location, logger *slog.Logger) http.Handler {
	server := &Server{querier: querier, loc: loc, logger: logger}

	mux := http.NewServeMux()
	// 三个只读 GET，路径不带版本段
	mux.HandleFunc("GET /api/context", server.handleContext)
	mux.HandleFunc("GET /api/meta", server.handleMeta)
	mux.HandleFunc("GET /api/query", server.handleQuery)
	return mux
}

// handleContext 返回今天的时间位置。
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newRequestID()

	result, err := s.querier.Context(r.Context())
	if err != nil {
		s.writeServiceError(w, requestID, "/api/context", err, started)
		return
	}

	body := contextBody{
		Date:       s.formatDate(result.Date),
		Term:       result.Term,
		Week:       result.Week,
		Weekday:    result.Weekday,
		InCalendar: result.InCalendar,
	}
	s.writeData(w, requestID, body)
	s.logServed(requestID, "/api/context", CodeOK, started, slog.LevelInfo)
}

// handleMeta 返回数据版本、字典、节次轴与逐周新鲜度。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newRequestID()

	result, err := s.querier.Meta(r.Context())
	if err != nil {
		s.writeServiceError(w, requestID, "/api/meta", err, started)
		return
	}

	s.writeData(w, requestID, s.metaBody(result))
	s.logServed(requestID, "/api/meta", CodeOK, started, slog.LevelInfo)
}

// handleQuery 处理空教室与全天状态两种视图。
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newRequestID()
	values := r.URL.Query()

	params, err := query.ParseParams(query.RawParams{
		View:       values.Get("view"),
		Keyword:    values.Get("keyword"),
		DateOffset: values.Get("date_offset"),
		StartNode:  values.Get("start_node"),
		EndNode:    values.Get("end_node"),
		Limit:      values.Get("limit"),
		Offset:     values.Get("offset"),
	})
	// 参数不合法直接返回 40001，不往下查库
	if err != nil {
		s.writeServiceError(w, requestID, "/api/query", err, started)
		return
	}

	result, err := s.querier.Query(r.Context(), params)
	if err != nil {
		s.writeServiceError(w, requestID, "/api/query", err, started)
		return
	}

	body := queryBody{
		Total:     result.Total,
		Items:     []any{},
		Date:      s.formatDate(result.Date),
		Week:      result.Week,
		DayOfWeek: result.Weekday,
	}
	// 两种视图的 items 形状不同，按视图分别组装，空结果也要给出空数组
	if params.View == query.ViewAvailability {
		items := make([]availabilityItem, 0, len(result.Availability))
		for _, item := range result.Availability {
			items = append(items, availabilityItem{ID: item.ID, Name: item.Name})
		}
		body.Items = items
	} else {
		items := make([]dayItem, 0, len(result.Day))
		for _, item := range result.Day {
			statuses := make(map[string]string, len(item.Statuses))
			for node, stateKey := range item.Statuses {
				statuses[node] = string(stateKey)
			}
			items = append(items, dayItem{ID: item.ID, Name: item.Name, Statuses: statuses})
		}
		body.Items = items
	}

	s.writeData(w, requestID, body)
	s.logServed(requestID, "/api/query", CodeOK, started, slog.LevelInfo)
}

// writeServiceError 把业务层的错误类型映射成契约里的响应码。
func (s *Server) writeServiceError(w http.ResponseWriter, requestID, path string, err error, started time.Time) {
	var paramError query.ParamError
	// 参数错误：前端要提示用户怎么改
	if errors.As(err, &paramError) {
		s.writeCode(w, requestID, CodeBadRequest, paramError.Message)
		s.logServed(requestID, path, CodeBadRequest, started, slog.LevelInfo)
		return
	}

	var noDataError query.NoDataError
	// 无数据：不是故障，属于“数据未收录”
	if errors.As(err, &noDataError) {
		s.writeCode(w, requestID, CodeNoData, noDataError.Message)
		s.logServed(requestID, path, CodeNoData, started, slog.LevelInfo)
		return
	}

	// 其余都是内部错误，细节只进日志，不回给客户端
	s.logger.Error("查询失败", "request_id", requestID, "path", path, "error", err)
	s.writeCode(w, requestID, CodeInternal, "internal error")
	s.logServed(requestID, path, CodeInternal, started, slog.LevelError)
}

// formatDate 按契约输出 YYYY-MM-DD。
func (s *Server) formatDate(value time.Time) string {
	return value.In(s.loc).Format("2006-01-02")
}

// formatTime 按契约输出带时区的 RFC3339。
func (s *Server) formatTime(value time.Time) string {
	return value.In(s.loc).Format(time.RFC3339)
}
