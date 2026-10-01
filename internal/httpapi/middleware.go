package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request_id"

type userCtxKey struct{}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			buf := make([]byte, 12)
			if _, err := rand.Read(buf); err != nil {
				reqID = fmt.Sprintf("fallback-%d", time.Now().UnixNano())
			} else {
				reqID = hex.EncodeToString(buf)
			}
		}
		w.Header().Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequestIDFrom(ctx context.Context) string {
	if val, ok := ctx.Value(requestIDKey).(string); ok {
		return val
	}
	return ""
}

// WithUserID оборачивает контекст с верифицированным ID пользователя
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userCtxKey{}, userID)
}

// UserIDFromContext извлекает ID аутентифицированного пользователя
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userCtxKey{}).(int64)
	return id, ok && id > 0
}

func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := RequestIDFrom(r.Context())

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)

			duration := time.Since(start)
			logger.InfoContext(r.Context(), "http request completed",
				slog.String("request_id", reqID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Duration("duration", duration),
			)
		})
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}
