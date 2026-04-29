package jwt

import (
	"errors"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("无效的 token")
	ErrExpiredToken = errors.New("token 已过期")
)

// Claims 自定义 JWT 声明
type Claims struct {
	Username string `json:"username"`
	jwtlib.RegisteredClaims
}

// Manager JWT 管理器
type Manager struct {
	secret []byte
	expiry time.Duration
}

// NewManager 创建 JWT 管理器
func NewManager(secret string, expiry time.Duration) *Manager {
	return &Manager{
		secret: []byte(secret),
		expiry: expiry,
	}
}

// Generate 生成 JWT token
func (m *Manager) Generate(username string) (string, error) {
	now := time.Now()
	claims := Claims{
		Username: username,
		RegisteredClaims: jwtlib.RegisteredClaims{
			ExpiresAt: jwtlib.NewNumericDate(now.Add(m.expiry)),
			IssuedAt:  jwtlib.NewNumericDate(now),
			Issuer:    "easy-qfnu-kjs",
		},
	}

	token := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

// Parse 解析并验证 JWT token
func (m *Manager) Parse(tokenStr string) (*Claims, error) {
	token, err := jwtlib.ParseWithClaims(tokenStr, &Claims{}, func(t *jwtlib.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwtlib.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return m.secret, nil
	})
	if err != nil {
		if errors.Is(err, jwtlib.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
