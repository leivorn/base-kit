package middleware

import (
	"errors"
	"strings"
	"time"

	"github.com/leivorn/base-kit/config"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

// token 类型：access 走 Authorization 头，refresh 只放 HttpOnly cookie。
// 两者用同一个密钥签名，必须靠 typ 区分——否则 30 天有效的 refresh token 可以直接当
// access token 用，access token 也能塞进 cookie 无限续签，短期 token 形同虚设。
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

var ErrTokenTypeMismatch = errors.New("token type mismatch")

type Claims struct {
	UserID   uint     `json:"userId"`
	Username string   `json:"username"`
	Roles    []string `json:"roles"` // 仅供前端展示；服务端鉴权以数据库为准，见 JWTAuth
	Type     string   `json:"typ"`
	jwt.RegisteredClaims
}

func GenerateAccessToken(userID uint, username string, roles []string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Roles:    roles,
		Type:     TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.C.JWT.Expire)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.C.JWT.Secret))
}

func GenerateRefreshToken(userID uint, username string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Type:     TokenTypeRefresh,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.C.JWT.RefreshExpire)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.C.JWT.Secret))
}

// ParseToken 校验签名与有效期，并要求 token 类型与 wantType 一致。
// 没有 typ 的旧 token 一律视为类型不符（升级后需重新登录一次）。
func ParseToken(tokenString, wantType string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(config.C.JWT.Secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, jwt.ErrSignatureInvalid
	}
	if claims.Type != wantType {
		return nil, ErrTokenTypeMismatch
	}
	return claims, nil
}

// TokenRevokedByPasswordChange 判断 token 是否签发于最近一次改密之前。
// jwt 的 iat 是秒级，changedAt 先截断到秒再比较：改密后同一秒内重新登录拿到的新 token
// 仍然有效；改密前签发的 token 在该秒之前，全部拒绝。
func TokenRevokedByPasswordChange(issuedAt *jwt.NumericDate, changedAt *time.Time) bool {
	if changedAt == nil {
		return false
	}
	if issuedAt == nil {
		return true
	}
	return issuedAt.Time.Before(changedAt.Truncate(time.Second))
}

func unauthorized(c *fiber.Ctx) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"code":    -1,
		"data":    nil,
		"error":   "Unauthorized Exception",
		"message": "Unauthorized Exception",
	})
}

// JWTAuth 校验 access token，并以数据库为准装配请求上下文：
// 用户是否存在/启用、当前启用的角色、改密后旧 token 作废。
// token 里的 roles claim 只是签发时的快照，不再作为鉴权依据——否则禁用用户、调整角色
// 都要等 token 过期（默认 7 天）才生效。查询结果带 TTL 缓存，见 user_cache.go。
//
// kit 已把它挂在 /admin 前缀上，/admin 下的路由不用再挂；同一请求里只校验一次，见 request_once.go。
func JWTAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if c.Locals(jwtAuthDone) != nil {
			return c.Next()
		}

		auth := c.Get("Authorization")
		if auth == "" {
			return unauthorized(c)
		}

		tokenStr := strings.TrimPrefix(auth, "Bearer ")
		claims, err := ParseToken(tokenStr, TokenTypeAccess)
		if err != nil {
			return unauthorized(c)
		}

		entry, err := loadUserAuth(claims.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return unauthorized(c)
			}
			// 数据库抖动不是鉴权失败：给 500 让前端重试，不能让它走"401 → 退出登录"
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"code":    -1,
				"data":    nil,
				"error":   "Failed to load user",
				"message": "Failed to load user",
			})
		}
		if entry.status != 1 {
			return unauthorized(c)
		}
		if TokenRevokedByPasswordChange(claims.IssuedAt, entry.passwordChangedAt) {
			return unauthorized(c)
		}

		c.Locals("userId", claims.UserID)
		c.Locals("username", entry.username)
		c.Locals("roles", entry.roles)
		c.Locals(jwtAuthDone, true)
		return c.Next()
	}
}
