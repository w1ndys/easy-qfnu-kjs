package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/internal/service"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	classroomService *service.ClassroomService
	statsService     *service.StatsService
}

func NewHandler(cs *service.ClassroomService, ss *service.StatsService) *Handler {
	return &Handler{classroomService: cs, statsService: ss}
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
	var req model.QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	// 简单的校验
	if req.BuildingName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入教学楼名称"})
		return
	}
	if req.StartNode == "" || req.EndNode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择起始和终止节次"})
		return
	}

	resp, err := h.classroomService.GetEmptyClassrooms(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 异步记录搜索统计（含完整参数和结果数量）
	if h.statsService != nil {
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

	c.JSON(http.StatusOK, resp)
}

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
