package middleware

import (
	"net/http"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/jwt"
	"github.com/gin-gonic/gin"
)

// JWTAuth 返回 JWT 认证中间件
func JWTAuth(jwtManager *jwt.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少认证信息"})
			c.Abort()
			return
		}

		// 期望格式: "Bearer <token>"
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "认证格式错误"})
			c.Abort()
			return
		}

		claims, err := jwtManager.Parse(parts[1])
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "认证失败: " + err.Error()})
			c.Abort()
			return
		}

		// 将用户名存入上下文
		c.Set("admin_username", claims.Username)
		c.Next()
	}
}
