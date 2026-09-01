package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/apikeys"
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
			if user, ok := service.findUserByEmail(claims.Email); ok {
				if !service.HasPermissionForOrg(user, claims.OrganizationID, permission) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if !strings.EqualFold(claims.Role, "") {
				if !service.HasPermission(&User{Organization: claims.OrganizationID, Role: claims.Role}, permission) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "user not found", http.StatusUnauthorized)
		})
	}
}

func RequireAuthOrAPIKey(service *Service, keyService *apikeys.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// allow API key via query param for browser EventSource connections
			if apiKey := strings.TrimSpace(r.URL.Query().Get("api_key")); apiKey != "" {
				if keyService == nil {
					http.Error(w, "missing api key service", http.StatusUnauthorized)
					return
				}
				key, ok := keyService.Validate(apiKey)
				if !ok || key == nil {
					http.Error(w, "invalid api key", http.StatusUnauthorized)
					return
				}
				ctx := context.WithValue(r.Context(), userContextKey, UserClaims{
					UserID:         key.UserID,
					OrganizationID: key.OrgID,
					Email:          key.UserID,
					Role:           "OWNER",
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if authorization := r.Header.Get("Authorization"); authorization != "" {
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
				return
			}

			if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
				if keyService == nil {
					http.Error(w, "missing api key service", http.StatusUnauthorized)
					return
				}
				key, ok := keyService.Validate(apiKey)
				if !ok || key == nil {
					http.Error(w, "invalid api key", http.StatusUnauthorized)
					return
				}
				ctx := context.WithValue(r.Context(), userContextKey, UserClaims{
					UserID:         key.UserID,
					OrganizationID: key.OrgID,
					Email:          key.UserID,
					Role:           "OWNER",
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			http.Error(w, "missing authorization header", http.StatusUnauthorized)
		})
	}
}

func RequireOrganizationAccess(requiredOrgID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := ExtractClaims(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if strings.TrimSpace(requiredOrgID) != "" && requiredOrgID != claims.OrganizationID {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
