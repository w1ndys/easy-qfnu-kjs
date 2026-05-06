package v1

import (
	"crypto/subtle"
	"net/http"
	"strconv"

	"github.com/W1ndys/easy-qfnu-kjs/internal/model"
	"github.com/W1ndys/easy-qfnu-kjs/internal/service"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/jwt"
	"github.com/gin-gonic/gin"
)

// AdminHandler 管理后台 API 处理器
type AdminHandler struct {
	announcementService *service.AnnouncementService
	apiConfigService    *service.APIConfigService
	jwtManager          *jwt.Manager
	adminUsername       string
	adminPassword       string
}

// NewAdminHandler 创建管理后台处理器
func NewAdminHandler(
	as *service.AnnouncementService,
	acs *service.APIConfigService,
	jm *jwt.Manager,
	username, password string,
) *AdminHandler {
	return &AdminHandler{
		announcementService: as,
		apiConfigService:    acs,
		jwtManager:          jm,
		adminUsername:       username,
		adminPassword:       password,
	}
}

// Login 管理员登录
func (h *AdminHandler) Login(c *gin.Context) {
	var req model.AdminLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	// 使用常量时间比较防止时序攻击
	usernameMatch := subtle.ConstantTimeCompare([]byte(req.Username), []byte(h.adminUsername))
	passwordMatch := subtle.ConstantTimeCompare([]byte(req.Password), []byte(h.adminPassword))
	if usernameMatch != 1 || passwordMatch != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}

	token, err := h.jwtManager.Generate(req.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成 token 失败"})
		return
	}

	c.JSON(http.StatusOK, model.AdminLoginResponse{Token: token})
}

// ---- 公告管理 CRUD ----

// ListAnnouncements 获取公告列表 (管理后台)
func (h *AdminHandler) ListAnnouncements(c *gin.Context) {
	list, err := h.announcementService.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取公告列表失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"announcements": list})
}

// CreateAnnouncement 创建公告
func (h *AdminHandler) CreateAnnouncement(c *gin.Context) {
	var req model.CreateAnnouncementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	a, err := h.announcementService.Create(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建公告失败"})
		return
	}

	c.JSON(http.StatusCreated, a)
}

// UpdateAnnouncement 更新公告
func (h *AdminHandler) UpdateAnnouncement(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的公告 ID"})
		return
	}

	var req model.UpdateAnnouncementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	a, err := h.announcementService.Update(id, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新公告失败"})
		return
	}
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "公告不存在"})
		return
	}

	c.JSON(http.StatusOK, a)
}

// DeleteAnnouncement 删除公告
func (h *AdminHandler) DeleteAnnouncement(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的公告 ID"})
		return
	}

	if err := h.announcementService.Delete(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "删除成功"})
}

// GetAPIConfig 获取 AI 与开放接口配置。
func (h *AdminHandler) GetAPIConfig(c *gin.Context) {
	if h.apiConfigService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "开放接口配置服务未初始化"})
		return
	}
	cfg, err := h.apiConfigService.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取开放接口配置失败"})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// UpdateAPIConfig 保存 AI 与开放接口配置。
func (h *AdminHandler) UpdateAPIConfig(c *gin.Context) {
	if h.apiConfigService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "开放接口配置服务未初始化"})
		return
	}
	var req model.APIConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}
	if req.OpenAPIEnabled && req.OpenAPIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "启用开放接口时必须配置授权 Key"})
		return
	}

	cfg, err := h.apiConfigService.Update(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存开放接口配置失败"})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// ListAIModels 从 OpenAI 兼容接口获取模型列表。
func (h *AdminHandler) ListAIModels(c *gin.Context) {
	if h.apiConfigService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "开放接口配置服务未初始化"})
		return
	}
	models, err := h.apiConfigService.ListModels(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, model.AIModelsResponse{Models: models})
}

// ---- 前台公开接口 ----

// GetPublicAnnouncements 获取前台公告列表 (无需认证)
func (h *AdminHandler) GetPublicAnnouncements(c *gin.Context) {
	list, err := h.announcementService.ListPublic()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取公告失败"})
		return
	}
	c.JSON(http.StatusOK, model.AnnouncementListResponse{Announcements: list})
}
