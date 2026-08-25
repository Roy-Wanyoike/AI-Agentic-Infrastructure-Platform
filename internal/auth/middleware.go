package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type contextKey string

const userContextKey contextKey = "auth.user"

type UserClaims struct {
	UserID         string
	OrganizationID string
	Email          string
	Role           string
}

func RequireAuth(service *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authorization := r.Header.Get("Authorization")
			if authorization == "" {
				http.Error(w, "missing authorization header", http.StatusUnauthorized)
				return
			}
			parts := strings.SplitN(authorization, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, "invalid authorization header", http.StatusUnauthorized)
				return
			}
			claims, err := service.ValidateToken(parts[1])
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, UserClaims{
				UserID:         claims.UserID,
				OrganizationID: claims.OrganizationID,
				Email:          claims.Email,
				Role:           claims.Role,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func ExtractClaims(ctx context.Context) (UserClaims, error) {
	claims, ok := ctx.Value(userContextKey).(UserClaims)
	if !ok {
		return UserClaims{}, errors.New("missing auth context")
	}
	return claims, nil
}

func RequirePermission(service *Service, permission Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := ExtractClaims(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			user, ok := service.findUserByEmail(claims.Email)
			if !ok {
				http.Error(w, "user not found", http.StatusUnauthorized)
				return
			}
			if !service.HasPermission(user, permission) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
