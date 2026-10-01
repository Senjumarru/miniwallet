package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Senjumarru/miniwallet/internal/domain"
	"github.com/golang-jwt/jwt/v5"
)

type JWTClaims struct {
	UserID int64 `json:"user_id"`
	jwt.RegisteredClaims
}

type JWTManager struct {
	secret []byte
}

func NewJWTManager(secret []byte) *JWTManager {
	return &JWTManager{secret: secret}
}

func (m *JWTManager) GenerateToken(userID int64, ttl time.Duration) (string, error) {
	claims := JWTClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}

func (m *JWTManager) VerifyToken(tokenString string) (int64, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil {
		return 0, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*JWTClaims)
	if !ok || !token.Valid {
		return 0, errors.New("invalid token claims")
	}

	if claims.UserID > 0 {
		return claims.UserID, nil
	}
	if claims.Subject != "" {
		if id, err := strconv.ParseInt(claims.Subject, 10, 64); err == nil && id > 0 {
			return id, nil
		}
	}
	return 0, errors.New("missing user_id in token claims")
}

// JWTMiddleware обеспечивает аутентификацию исключительно по проверенному JWT токену.
// Любые клиентские заголовки X-User-ID и поля тела игнорируются (п. 5.1).
func JWTMiddleware(jwtMgr *JWTManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				writeDomainError(w, domain.ErrUnauthorized)
				return
			}

			tokenStr := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			userID, err := jwtMgr.VerifyToken(tokenStr)
			if err != nil {
				writeDomainError(w, domain.ErrUnauthorized)
				return
			}

			ctx := WithUserID(r.Context(), userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
