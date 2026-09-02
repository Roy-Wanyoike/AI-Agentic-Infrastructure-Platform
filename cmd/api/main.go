package main

import (
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/config"
	"agentos/internal/database"
	"agentos/internal/logger"
	"agentos/internal/observability"
	"agentos/internal/organizations"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/streaming"
	"agentos/internal/usage"
)

// defaultJWTSecret is used until a JWT_SECRET setting is introduced; the HMAC
// token scheme itself is unchanged.
const defaultJWTSecret = "dev-secret"

// app bundles every service the API handlers need. When db is nil the app runs
// in zero-infrastructure mode with in-memory services (tests/dev default).
type app struct {
	cfg        config.Config
	logr       *slog.Logger
	db         *sql.DB
	authSvc    *auth.Service
	apiKeysSvc *apikeys.Service
	orgsSvc    *organizations.Service
	auditSvc   *audit.Service
	usageSvc   *usage.Service
	agentsSvc  *agents.Service
	runsSvc    *runs.Service
	queueSvc   *queue.Queue
	metricsSvc *observability.Metrics
	streamSvc  *streaming.Service
}

// newApp builds the service graph. When db is non-nil every service is
// constructed with a Postgres-backed store; otherwise the original in-memory
// services are used so the platform runs with zero infrastructure.
func newApp(cfg config.Config, logr *slog.Logger, db *sql.DB) *app {
	a := &app{
		cfg:        cfg,
		logr:       logr,
		db:         db,
		queueSvc:   queue.NewQueue(),
		metricsSvc: observability.NewMetrics(),
		streamSvc:  streaming.NewService(),
	}
	if db != nil {
		a.authSvc = auth.NewServiceWithStore(defaultJWTSecret, auth.NewPostgresStore(db))
		a.apiKeysSvc = apikeys.NewServiceWithStore(apikeys.NewPostgresStore(db))
		a.orgsSvc = organizations.NewServiceWithStore(organizations.NewPostgresStore(db))
		a.auditSvc = audit.NewServiceWithStore(audit.NewPostgresStore(db))
		a.usageSvc = usage.NewServiceWithStore(usage.NewPostgresStore(db))
		a.agentsSvc = agents.NewServiceWithStore(agents.NewPostgresStore(db))
		a.runsSvc = runs.NewServiceWithStore(runs.NewPostgresStore(db))
		a.logr.Info("postgres stores enabled")
	} else {
		a.authSvc = auth.NewService(defaultJWTSecret)
		a.apiKeysSvc = apikeys.NewService()
		a.orgsSvc = organizations.NewService()
		a.auditSvc = audit.NewService()
		a.usageSvc = usage.NewService()
		a.agentsSvc = agents.NewService()
		a.runsSvc = runs.NewService()
		// create a dev API key for local worker polling convenience (only
		// possible in-memory: the api_keys FK requires a real organization row)
		if key, err := a.apiKeysSvc.Create("org-demo", "dev-user", "dev-key"); err != nil {
			a.logr.Warn("dev api key creation failed", "error", err)
		} else {
			a.logr.Info("dev api key created", "api_key", key.Value)
		}
	}
	// wire runs service to streaming service so run status updates are published
	a.runsSvc.SetStreamer(a.streamSvc)
	// expose to handlers for backwards-compatible wiring in tests
	runsServiceVar = a.runsSvc
	return a
}

// routes builds the HTTP routing surface. Versioned API routes are registered
// once on an internal mux and mounted under BOTH the legacy /v1 prefix and the
// canonical /api/v1 prefix; /healthz and /readyz stay unversioned.
func (a *app) routes() http.Handler {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/auth/register", registerHandler(a.authSvc))
	apiMux.HandleFunc("/auth/login", loginHandler(a.authSvc))
	apiMux.Handle("/agents", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionAgentsRead)(http.HandlerFunc(listAgentsHandler(a.agentsSvc)))))
	apiMux.Handle("/agents/create", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionAgentsWrite)(http.HandlerFunc(createAgentHandler(a.agentsSvc, a.auditSvc)))))
	apiMux.Handle("/agents/", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionAgentsRead)(http.HandlerFunc(agentDetailHandler(a.agentsSvc)))))
	apiMux.Handle("/runs", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// require read permission for listing
			auth.RequirePermission(a.authSvc, auth.PermissionRunsRead)(http.HandlerFunc(listRunsHandler(a.runsSvc))).ServeHTTP(w, r)
			return
		}
		// default to create (POST)
		auth.RequirePermission(a.authSvc, auth.PermissionRunsExecute)(http.HandlerFunc(createRunHandler(a.queueSvc, a.auditSvc))).ServeHTTP(w, r)
	})))
	// queue pull endpoint for workers to pull tasks (dev-only)
	apiMux.Handle("/queue/pull", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionRunsExecute)(http.HandlerFunc(queuePullHandler(a.queueSvc)))))
	apiMux.Handle("/runs/", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionRunsRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := trimRoutePrefix(r.URL.Path, "/runs/")
		switch {
		case strings.HasSuffix(rest, "/events"):
			runEventsHandler(a.streamSvc).ServeHTTP(w, r)
		case strings.HasSuffix(rest, "/steps"):
			runStepsHandler(a.runsSvc).ServeHTTP(w, r)
		default:
			getRunHandler(a.runsSvc).ServeHTTP(w, r)
		}
	}))))
	apiMux.Handle("/metrics", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionRunsRead)(http.HandlerFunc(metricsHandler(a.metricsSvc, a.queueSvc)))))
	apiMux.HandleFunc("/", serviceInfoHandler)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	// The authenticated API is served under /api/v1 (canonical, used by the
	// frontend) and /v1 (legacy clients); StripPrefix lets the inner mux serve
	// both without duplicating route registrations.
	mux.Handle("/v1/", http.StripPrefix("/v1", apiMux))
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", apiMux))
	mux.HandleFunc("/", serviceInfoHandler)

	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	// wrap with a permissive CORS handler for local development
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-API-Key,api_key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)

	// Attempt a Postgres connection when DATABASE_URL / POSTGRES_* are set.
	// The platform must keep running with zero infrastructure, so any failure
	// falls back to in-memory stores.
	var db *sql.DB
	if dsn := database.DSNFromEnv(); dsn != "" {
		conn, err := database.Connect(dsn)
		if err != nil {
			logr.Warn("database unavailable, using in-memory stores", "error", err.Error())
		} else {
			db = conn
		}
	} else {
		logr.Warn("database unavailable, using in-memory stores", "error", "DATABASE_URL/POSTGRES_* env vars are not set")
	}
	if db != nil {
		defer db.Close()
	}

	application := newApp(cfg, logr, db)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.API.Port),
		Handler:      application.routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	logr.Info("agentos api starting", "port", cfg.API.Port, "env", cfg.Env)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("listen failed: %v", err)
		os.Exit(1)
	}
}
