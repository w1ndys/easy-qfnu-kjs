package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/internal/service"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	classroomService *service.ClassroomService
	statsService     *service.StatsService
	apiConfigService *service.APIConfigService
}

func NewHandler(cs *service.ClassroomService, ss *service.StatsService, acs *service.APIConfigService) *Handler {
	return &Handler{classroomService: cs, statsService: ss, apiConfigService: acs}
}

// hashUA 对 User-Agent 做 SHA256 并返回前 16 个十六进制字符 (与 middleware.RateLimiter 的哈希策略一致)
func hashUA(ua string) string {
	if ua == "" {
		return ""
	}
	h := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(h[:8])
}

// GetStatus 返回系统状态，包括是否在教学周历内
func (h *Handler) GetStatus(c *gin.Context) {
	cal := service.GetCalendarService()
	if cal == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":                "日历服务未初始化",
			"in_teaching_calendar": false,
			"current_week":         0,
			"current_term":         "",
		})
		return
	}

	inCalendar := cal.IsInTeachingCalendar()
	c.JSON(http.StatusOK, gin.H{
		"in_teaching_calendar": inCalendar,
		"current_week":         cal.GetBaseWeek(),
		"current_term":         cal.GetCurrentYearStr(),
		"has_permission":       cal.HasPermission(),
	})
}

func (h *Handler) QueryClassrooms(c *gin.Context) {
	h.queryClassrooms(c, true)
}

// AIQueryClassrooms 前台自然语言查询接口，使用普通频率限制。
func (h *Handler) AIQueryClassrooms(c *gin.Context) {
	h.aiQueryClassrooms(c, true)
}

// OpenQueryClassrooms 开放直接查询接口，不挂载高频限制。
func (h *Handler) OpenQueryClassrooms(c *gin.Context) {
	if !h.validateOpenAPIKey(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "开放接口授权失败"})
		return
	}
	h.queryClassrooms(c, false)
}

// OpenAIQueryClassrooms 开放 AI 自然语言查询接口，不挂载高频限制。
func (h *Handler) OpenAIQueryClassrooms(c *gin.Context) {
	if !h.validateOpenAPIKey(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "开放接口授权失败"})
		return
	}
	h.aiQueryClassrooms(c, false)
}

func (h *Handler) aiQueryClassrooms(c *gin.Context, recordStats bool) {
	if h.apiConfigService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI 服务未初始化"})
		return
	}

	var req model.AIQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}
	parsed, err := h.apiConfigService.ParseNaturalLanguage(c.Request.Context(), req.Text)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI 解析失败，请完善精确的描述语言，或使用直接查询接口", "detail": err.Error()})
		return
	}
	if parsed.Confidence != "high" || parsed.BuildingName == "" || parsed.StartNode == "" || parsed.EndNode == "" {
		msg := parsed.Reason
		if msg == "" {
			msg = "请提供明确的教学楼名称、目标日期和节次范围"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI 解析失败，请完善精确的描述语言，或使用直接查询接口", "detail": msg, "parsed": parsed})
		return
	}

	query := model.QueryRequest{
		BuildingName: parsed.BuildingName,
		DateOffset:   parsed.DateOffset,
		StartNode:    parsed.StartNode,
		EndNode:      parsed.EndNode,
	}
	resp, err := h.runClassroomQuery(c, query, recordStats)
	if err != nil {
		status := http.StatusInternalServerError
		if _, ok := err.(errBadRequest); ok {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, model.AIQueryResponse{Parsed: *parsed, Result: resp})
}

func (h *Handler) queryClassrooms(c *gin.Context, recordStats bool) {
	var req model.QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	resp, err := h.runClassroomQuery(c, req, recordStats)
	if err != nil {
		status := http.StatusInternalServerError
		if _, ok := err.(errBadRequest); ok {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) runClassroomQuery(c *gin.Context, req model.QueryRequest, recordStats bool) (*model.ClassroomResponse, error) {
	if req.BuildingName == "" {
		return nil, errBadRequest("请输入教学楼名称")
	}
	if req.StartNode == "" || req.EndNode == "" {
		return nil, errBadRequest("请选择起始和终止节次")
	}

	resp, err := h.classroomService.GetEmptyClassrooms(req)
	if err != nil {
		return nil, err
	}
	if recordStats && h.statsService != nil {
		resultCount := len(resp.Classrooms)
		go h.statsService.RecordQuery(model.QueryRecord{
			Keyword:     req.BuildingName,
			DateOffset:  req.DateOffset,
			StartNode:   req.StartNode,
			EndNode:     req.EndNode,
			ResultCount: resultCount,
			IP:          c.ClientIP(),
			UAHash:      hashUA(c.GetHeader("User-Agent")),
		})
	}
	return resp, nil
}

func (h *Handler) validateOpenAPIKey(c *gin.Context) bool {
	if h.apiConfigService == nil {
		return false
	}
	key := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if key == "" {
		auth := strings.TrimSpace(c.GetHeader("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			key = strings.TrimSpace(auth[7:])
		}
	}
	return h.apiConfigService.ValidateOpenAPIKey(key)
}

type errBadRequest string

func (e errBadRequest) Error() string { return string(e) }

// QueryFullDayStatus 查询全天教室状态
func (h *Handler) QueryFullDayStatus(c *gin.Context) {
	var req model.FullDayQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	if req.BuildingName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入教学楼名称"})
		return
	}

	resp, err := h.classroomService.GetFullDayStatus(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 异步记录搜索统计（全天状态查询无节次参数）
	if h.statsService != nil {
		resultCount := len(resp.Classrooms)
		go h.statsService.RecordQuery(model.QueryRecord{
			Keyword:     req.BuildingName,
			DateOffset:  req.DateOffset,
			ResultCount: resultCount,
			IP:          c.ClientIP(),
			UAHash:      hashUA(c.GetHeader("User-Agent")),
		})
	}

	c.JSON(http.StatusOK, resp)
}

// GetStats 获取查询统计数据
func (h *Handler) GetStats(c *gin.Context) {
	if h.statsService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "统计服务未初始化"})
		return
	}

	stats, err := h.statsService.GetStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取统计数据失败"})
		return
	}

	c.JSON(http.StatusOK, stats)
}

// GetTopBuildings 获取搜索排行前 N 的热门查询组合
func (h *Handler) GetTopBuildings(c *gin.Context) {
	if h.statsService == nil {
		c.JSON(http.StatusOK, &model.TopQueriesResponse{Queries: []model.TopQueryItem{}})
		return
	}

	queries, err := h.statsService.GetTopQueries(5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取热门搜索组合失败"})
		return
	}

	if queries == nil {
		queries = []model.TopQueryItem{}
	}

	c.JSON(http.StatusOK, &model.TopQueriesResponse{Queries: queries})
}

// GetDashboard 获取数据大屏综合统计数据
func (h *Handler) GetDashboard(c *gin.Context) {
	if h.statsService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "统计服务未初始化"})
		return
	}

	timeRange := c.DefaultQuery("range", "today")
	days := 0
	if timeRange != "today" && timeRange != "week" && timeRange != "month" && timeRange != "custom" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的时间范围，可选: today, week, month, custom"})
		return
	}
	if timeRange == "custom" {
		parsedDays, err := strconv.Atoi(c.DefaultQuery("days", ""))
		if err != nil || parsedDays < 1 || parsedDays > 365 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "自定义天数必须是 1 到 365 之间的整数"})
			return
		}
		days = parsedDays
	}

	// 获取客户端时区偏移（分钟），默认 480（UTC+8，中国标准时间）
	tzOffsetMin := 480
	if tzStr := c.DefaultQuery("tz_offset", ""); tzStr != "" {
		if parsed, err := strconv.Atoi(tzStr); err == nil && parsed >= -720 && parsed <= 840 {
			tzOffsetMin = parsed
		}
	}

	data, err := h.statsService.GetDashboardData(timeRange, days, tzOffsetMin)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取大屏数据失败"})
		return
	}

	c.JSON(http.StatusOK, data)
}
