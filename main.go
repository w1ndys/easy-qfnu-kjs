package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"

	v1 "github.com/W1ndys/easy-qfnu-kjs/internal/api/v1"
	"github.com/W1ndys/easy-qfnu-kjs/internal/middleware"
	"github.com/W1ndys/easy-qfnu-kjs/internal/service"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/cas"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/jwt"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	// 加载 .env
	_ = godotenv.Load()

	// 设置 Gin 模式
	if mode := os.Getenv("GIN_MODE"); mode != "" {
		gin.SetMode(mode)
	}

	// 1. 初始化 CAS 客户端
	// 注意：实际生产环境需要配置账号密码，用于获取 Session
	username := os.Getenv("QFNU_USER")
	password := os.Getenv("QFNU_PASS")

	// 兼容 main.go 原有逻辑，也尝试读取 QFNU_USERNAME/PASSWORD
	if username == "" {
		username = os.Getenv("QFNU_USERNAME")
	}
	if password == "" {
		password = os.Getenv("QFNU_PASSWORD")
	}

	if username == "" || password == "" {
		logger.Warn("未设置 QFNU_USER/QFNU_PASS。由于缺少会话，后端查询可能会失败。")
	}

	client, err := cas.NewClient(cas.WithTimeout(30 * time.Second))
	if err != nil {
		logger.Fatal("无法创建 CAS 客户端：%v", err)
	}

	// 尝试登录以获取 Session
	if username != "" {
		logger.Info("正在尝试登录 QFNU CAS...")
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		err := client.Login(ctx, username, password)
		if err != nil {
			logger.Warn("登录失败：%v。程序将继续运行，但查询可能会失败。", err)
		} else {
			logger.Info("登录成功。")
		}
	}

	// 2. 初始化服务
	if err := service.InitCalendarService(client); err != nil {
		logger.Warn("初始化日历服务失败：%v。日历功能可能不准确。", err)
	}

	// 启动每日 00:01 自动刷新周次
	if cal := service.GetCalendarService(); cal != nil {
		cal.StartDailyRefresh()
	}

	classroomService := service.NewClassroomService(client)

	// 初始化统计服务
	statsDBPath := os.Getenv("STATS_DB_PATH")
	if statsDBPath == "" {
		statsDBPath = "data/stats.db"
	}

	statsService, err := service.NewStatsService(statsDBPath)
	if err != nil {
		logger.Warn("初始化统计服务失败：%v。统计功能将不可用。", err)
	}
	if statsService != nil {
		defer statsService.Close()
	}

	// 初始化公告服务（复用 StatsService 的 SQLite 连接，生命周期由 StatsService 管理）
	var announcementService *service.AnnouncementService
	var apiConfigService *service.APIConfigService
	if statsService != nil {
		as, err := service.NewAnnouncementService(statsService.DB())
		if err != nil {
			logger.Warn("初始化公告服务失败：%v。公告功能将不可用。", err)
		} else {
			announcementService = as
		}
		acs, err := service.NewAPIConfigService(statsService.DB())
		if err != nil {
			logger.Warn("初始化开放接口配置服务失败：%v。开放接口功能将不可用。", err)
		} else {
			apiConfigService = acs
		}
	}

	// 初始化 JWT 管理器
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		if os.Getenv("GIN_MODE") == "release" {
			logger.Fatal("生产环境必须设置 JWT_SECRET 环境变量")
		}
		// 开发环境未配置时自动生成随机密钥（重启后旧 token 失效）
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			logger.Fatal("生成随机 JWT 密钥失败：%v", err)
		}
		jwtSecret = hex.EncodeToString(buf)
		logger.Warn("未设置 JWT_SECRET，已自动生成随机密钥。重启后管理员需重新登录。")
	}
	jwtManager := jwt.NewManager(jwtSecret, 24*time.Hour)

	// 读取管理员账号
	adminUser := os.Getenv("ADMIN_USERNAME")
	adminPass := os.Getenv("ADMIN_PASSWORD")
	if adminUser == "" || adminPass == "" {
		logger.Warn("未设置 ADMIN_USERNAME/ADMIN_PASSWORD，管理后台将不可用。")
	}

	var adminHandler *v1.AdminHandler
	if announcementService != nil && adminUser != "" && adminPass != "" {
		adminHandler = v1.NewAdminHandler(announcementService, apiConfigService, jwtManager, adminUser, adminPass)
	}

	apiHandler := v1.NewHandler(classroomService, statsService, apiConfigService)

	// 3. 设置 Gin
	r := gin.Default()
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.ForwardedByClientIP = true
	r.RemoteIPHeaders = []string{"X-Forwarded-For", "X-Real-IP"}

	// 搜索接口速率限制：基于 IP + User-Agent，5 秒内只能查询一次
	searchRateLimiter := middleware.NewRateLimiter(5 * time.Second)

	// API 路由
	api := r.Group("/api/v1")
	{
		api.GET("/status", apiHandler.GetStatus)
		api.POST("/query", searchRateLimiter.Middleware(), apiHandler.QueryClassrooms)
		api.POST("/ai-query", searchRateLimiter.Middleware(), apiHandler.AIQueryClassrooms)
		api.POST("/query-full-day", searchRateLimiter.Middleware(), apiHandler.QueryFullDayStatus)
		api.POST("/open/query", apiHandler.OpenQueryClassrooms)
		api.POST("/open/ai-query", apiHandler.OpenAIQueryClassrooms)
		api.GET("/stats", apiHandler.GetStats)
		api.GET("/top-buildings", apiHandler.GetTopBuildings)
		api.GET("/dashboard", apiHandler.GetDashboard)

		// 前台公告公开接口
		if adminHandler != nil {
			api.GET("/announcements", adminHandler.GetPublicAnnouncements)
		}
	}

	// 管理后台 API
	if adminHandler != nil {
		api.POST("/admin/login", adminHandler.Login)

		admin := api.Group("/admin")
		admin.Use(middleware.JWTAuth(jwtManager))
		{
			admin.GET("/announcements", adminHandler.ListAnnouncements)
			admin.POST("/announcements", adminHandler.CreateAnnouncement)
			admin.PUT("/announcements/:id", adminHandler.UpdateAnnouncement)
			admin.DELETE("/announcements/:id", adminHandler.DeleteAnnouncement)
			admin.GET("/api-config", adminHandler.GetAPIConfig)
			admin.PUT("/api-config", adminHandler.UpdateAPIConfig)
			admin.POST("/api-config/ai-prompt/default", adminHandler.ResetAIPrompt)
			admin.GET("/ai-models", adminHandler.ListAIModels)
		}
	}

	// 启动
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Info("服务器正在启动，监听地址：http://localhost:%s", port)
	if err := r.Run(":" + port); err != nil {
		logger.Fatal("%v", err)
	}
}
