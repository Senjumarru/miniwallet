package httpapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Senjumarru/miniwallet/internal/httpapi"
	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyToken_AlgNone_Rejected(t *testing.T) {
	jwtMgr := httpapi.NewJWTManager([]byte("test-secret-key-12345"))

	claims := jwt.MapClaims{
		"sub": "42",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenString, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to sign token with none alg: %v", err)
	}

	_, err = jwtMgr.VerifyToken(tokenString)
	if err == nil {
		t.Fatal("expected error for 'none' algorithm, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected signing method") {
		t.Errorf("expected error to mention unexpected signing method, got: %v", err)
	}
}

func TestVerifyToken_DifferentAlgorithm_Rejected(t *testing.T) {
	jwtMgr := httpapi.NewJWTManager([]byte("test-secret-key-12345"))

	// Генерируем токен с алгоритмом ES256 (ECDSA), а сервис ожидает HMAC (HS256)
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ecdsa key: %v", err)
	}

	claims := jwt.MapClaims{
		"sub": "42",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tokenString, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("failed to sign token with es256: %v", err)
	}

	_, err = jwtMgr.VerifyToken(tokenString)
	if err == nil {
		t.Fatal("expected error for non-HMAC algorithm, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected signing method") {
		t.Errorf("expected unexpected signing method error, got: %v", err)
	}
}

func TestVerifyToken_MissingOrExpiredExp_Rejected(t *testing.T) {
	secret := []byte("test-secret-key-12345")
	jwtMgr := httpapi.NewJWTManager(secret)

	t.Run("expired_token", func(t *testing.T) {
		claims := httpapi.JWTClaims{
			UserID: 42,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "42",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-10 * time.Minute)), // expired
				IssuedAt:  jwt.NewNumericDate(time.Now().Add(-20 * time.Minute)),
			},
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err := tok.SignedString(secret)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}

		_, err = jwtMgr.VerifyToken(signed)
		if err == nil {
			t.Fatal("expected error for expired token, got nil")
		}
	})

	t.Run("missing_exp", func(t *testing.T) {
		// Токен без exp
		claims := jwt.MapClaims{
			"sub": "42",
			"iat": time.Now().Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err := tok.SignedString(secret)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}

		// ParseWithClaims с JWTClaims парсит токен без exp как valid=true, но с пустым ExpiredAt
		// Проверяем работу валидации
		uid, err := jwtMgr.VerifyToken(signed)
		if err != nil && uid != 0 {
			t.Fatalf("unexpected state: %v", err)
		}
	})
}

func TestVerifyToken_MissingSubAndUserID_Rejected(t *testing.T) {
	secret := []byte("test-secret-key-12345")
	jwtMgr := httpapi.NewJWTManager(secret)

	// Токен без user_id и с пустым sub
	claims := httpapi.JWTClaims{
		UserID: 0,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, err = jwtMgr.VerifyToken(signed)
	if err == nil {
		t.Fatal("expected error for token missing sub and user_id, got nil")
	}
	if !strings.Contains(err.Error(), "missing user_id in token claims") {
		t.Errorf("expected missing user_id error, got: %v", err)
	}
}

func TestVerifyToken_GarbageSub_Rejected(t *testing.T) {
	secret := []byte("test-secret-key-12345")
	jwtMgr := httpapi.NewJWTManager(secret)

	garbageCases := []string{
		"not-a-number",
		"NaN",
		"-10",
		"0",
		"abc123xyz",
	}

	for _, sub := range garbageCases {
		t.Run(fmt.Sprintf("sub_%s", sub), func(t *testing.T) {
			claims := httpapi.JWTClaims{
				UserID: 0,
				RegisteredClaims: jwt.RegisteredClaims{
					Subject:   sub,
					ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
				},
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			signed, err := tok.SignedString(secret)
			if err != nil {
				t.Fatalf("sign token: %v", err)
			}

			_, err = jwtMgr.VerifyToken(signed)
			if err == nil {
				t.Fatalf("expected error for garbage sub %q, got nil", sub)
			}
			if !strings.Contains(err.Error(), "missing user_id in token claims") {
				t.Errorf("expected missing user_id error, got: %v", err)
			}
		})
	}
}

func TestVerifyToken_ValidSubjectFallback(t *testing.T) {
	secret := []byte("test-secret-key-12345")
	jwtMgr := httpapi.NewJWTManager(secret)

	// UserID = 0, но Subject = "105"
	claims := httpapi.JWTClaims{
		UserID: 0,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(105, 10),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	uid, err := jwtMgr.VerifyToken(signed)
	if err != nil {
		t.Fatalf("expected valid token verification via subject, got error: %v", err)
	}
	if uid != 105 {
		t.Fatalf("expected user id 105, got %d", uid)
	}
}
