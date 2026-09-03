package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	// tzdata ensures time.LoadLocation works in scratch/distroless containers.
	_ "time/tzdata"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/config"
	"agentos/internal/database"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
	"agentos/internal/events"
	"agentos/internal/httpx"
	"agentos/internal/logger"
	"agentos/internal/observability"
	"agentos/internal/organizations"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/runtime"
	"agentos/internal/scheduler"
	"agentos/internal/streaming"
	"agentos/internal/usage"
	"agentos/internal/webhooks"
	"agentos/internal/workflows"
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

	// wave-2 verticals
	wfSvc          *workflows.Service
	apSvc          *approvals.Service
	versionsSvc    *agents.VersionsService
	deploymentsSvc *deployments.Service
	evalSvc        *evaluations.Service
	evalRunner     *runtime.Runner
	schedSvc       *scheduler.Service
	whSvc          *webhooks.Service
	publisher      events.Publisher
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
		a.wfSvc = workflows.NewServiceWithStore(workflows.NewPostgresStore(db))
		a.apSvc = approvals.NewServiceWithStore(approvals.NewPostgresStore(db))
		a.versionsSvc = agents.NewVersionsServiceWithStore(a.agentsSvc, agents.NewVersionsPostgresStore(db))
		a.deploymentsSvc = deployments.NewServiceWithStore(deployments.NewPostgresStore(db), a.versionsSvc)
		a.schedSvc = scheduler.NewServiceWithStore(scheduler.NewPostgresStore(db))
		a.logr.Info("postgres stores enabled")
	} else {
		a.authSvc = auth.NewService(defaultJWTSecret)
		a.apiKeysSvc = apikeys.NewService()
		a.orgsSvc = organizations.NewService()
		a.auditSvc = audit.NewService()
		a.usageSvc = usage.NewService()
		a.agentsSvc = agents.NewService()
		a.runsSvc = runs.NewService()
		a.wfSvc = workflows.NewService()
		a.apSvc = approvals.NewService()
		a.versionsSvc = agents.NewVersionsService(a.agentsSvc)
		a.deploymentsSvc = deployments.NewService(a.versionsSvc)
		a.schedSvc = scheduler.NewService()
		// create a dev API key for local worker polling convenience (only
		// possible in-memory: the api_keys FK requires a real organization row)
		if key, err := a.apiKeysSvc.Create("org-demo", "dev-user", "dev-key"); err != nil {
			a.logr.Warn("dev api key creation failed", "error", err)
		} else {
			a.logr.Info("dev api key created", "api_key", key.Value)
		}
	}
	// wave-2: evaluation runner + service (deterministic offline mode unless a
	// provider is configured via env in the worker; the API runner stays offline)
	a.evalRunner = runtime.NewRunnerWithOptions(a.agentsSvc, nil)
	if db != nil {
		a.evalSvc = evaluations.NewServiceWithStore(evaluations.NewPostgresStore(db), evaluations.Deps{
			Agents: a.agentsSvc,
			Runner: a.evalRunner,
		})
	} else {
		a.evalSvc = evaluations.NewService(evaluations.Deps{
			Agents: a.agentsSvc,
			Runner: a.evalRunner,
		})
	}

	// wave-2: event publisher (NATS JetStream when AGENTOS_NATS_URL is set and
	// reachable; otherwise in-memory/noop fallbacks) + append-only audit trail
	a.publisher = events.NewFromEnv()
	if db != nil {
		a.publisher = events.NewAuditPublisher(events.NewPostgresStore(db), a.publisher)
	}

	// wave-2: webhooks service + delivery worker (single process: this API).
	if db != nil {
		a.whSvc = webhooks.NewServiceWithStore(webhooks.NewPostgresStore(db))
	} else {
		a.whSvc = webhooks.NewService()
	}
	a.whSvc.SetSigningKey(os.Getenv("AGENTOS_WEBHOOK_SIGNING_KEY"))
	if sub, ok := a.publisher.(events.Subscriber); ok {
		whWorker := webhooks.NewWorker(a.whSvc, sub, nil, logr)
		go func() {
			if err := whWorker.Run(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
				logr.Warn("webhook delivery worker stopped", "error", err.Error())
			}
		}()
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
		// default to create (POST); idempotency middleware honors Idempotency-Key
		idempotent := httpx.NewIdempotencyMiddleware(httpx.NewIdempotencyStoreFromDB(a.db))
		auth.RequirePermission(a.authSvc, auth.PermissionRunsExecute)(idempotent(http.HandlerFunc(createRunHandler(a.queueSvc, a.auditSvc)))).ServeHTTP(w, r)
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
	apiMux.Handle("/metrics", auth.RequireAuthOrAPIKey(a.authSvc, a.apiKeysSvc)(auth.RequirePermission(a.authSvc, auth.PermissionRunsRead)(http.HandlerFunc(metricsV2Handler(a.metricsSvc, a.queueSvc)))))

	// wave-2 verticals (workflows registration also mounts run control:
	// POST /runs/{id}/cancel|pause|resume)
	registerWorkflowsRoutes(apiMux, a.wfSvc, a.apSvc, a.runsSvc, a.queueSvc, a.authSvc, a.apiKeysSvc)
	registerVersionsRoutes(apiMux, a.versionsSvc, a.authSvc, a.apiKeysSvc)
	registerDeploymentsRoutes(apiMux, a.deploymentsSvc, a.authSvc, a.apiKeysSvc)
	registerEvaluationsRoutes(apiMux, a.evalSvc, a.authSvc, a.apiKeysSvc)
	registerSchedulesRoutes(apiMux, a.schedSvc, a.authSvc, a.apiKeysSvc, a.auditSvc)
	registerWebhooksRoutes(apiMux, a.whSvc, a.authSvc, a.apiKeysSvc, a.auditSvc)
	registerPoliciesRoutes(apiMux, newPoliciesService(a.db), a.authSvc, a.apiKeysSvc)

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

	// wave-2: global rate limiting (Redis when configured, in-memory fallback;
	// AGENTOS_RATE_LIMIT_RPM, default 120) outside CORS so 429s are JSON, and
	// request metrics outermost so every response (incl. 429s) is observed.
	limit, window := httpx.RateLimitFromEnv()
	rateLimit := httpx.NewRateLimitMiddleware(
		httpx.RedisClientFromEnv(),
		observability.NewRateLimiter(limit, window),
		limit, window,
	)
	return observability.MetricsMiddleware(a.metricsSvc, rateLimit(corsMiddleware(mux)))
}

func corsMiddleware(next http.Handler) http.Handler {
	// wrap with a permissive CORS handler for local development
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-API-Key,api_key,Idempotency-Key")
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

	// wave-2: scheduler trigger loop (runs in exactly one process; claims are
	// atomic so a second instance would be safe, just wasteful).
	schedPoll := scheduler.DefaultPollInterval
	if v := strings.TrimSpace(os.Getenv("AGENTOS_SCHEDULER_POLL_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			schedPoll = d
		}
	}
	schedWorker := scheduler.NewWorker(application.schedSvc, application.runsSvc, application.queueSvc, schedPoll)
	go schedWorker.Run(context.Background())
	logr.Info("scheduler trigger worker started", "poll_interval", schedPoll.String())

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
