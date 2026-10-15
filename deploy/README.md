# AgentOS Deployment Artifacts (issue #117)

Production-shaped Kubernetes manifests, a Helm chart, Kustomize overlays, and
Terraform modules for deploying AgentOS (api, worker, web) plus its data tier.

Everything here is **declarative artifacts only — zero Go code changes**.
Image placeholders are `ghcr.io/roy-wanyoike/agentos-{api,worker,web}:<tag>`,
built by `Dockerfile.api` / `Dockerfile.worker` / `Dockerfile.web` in the repo
root. Health probes use the **real** endpoints registered in `cmd/api/main.go`:
`/healthz` (liveness) and `/readyz` (readiness). The worker runs **no HTTP
server** (`Dockerfile.worker`), so it deliberately has **no HTTP probes** — do
not invent any.

---

## Architecture (reference in-cluster layout)

```
                          ┌──────────────────────────── Kubernetes (ns: agentos) ───────────────────────────┐
                          │                                                                                 │
  Internet ──TLS──▶ Ingress (nginx) ──┬── /           ▶ agentos-web (nginx:alpine, :80, Deployment x2)       │
                          │          └── /api (opt.) ▶ agentos-api  (distroless, :8080, Deployment x2, HPA)│
                          │                                       │            ▲                           │
                          │            agentos-web --/api proxy──▶│            │ status callbacks          │
                          │                                       ▼            │ (AGENTOS_API + API key)   │
                          │            agentos-worker (distroless, Deployment x2, HPA, no HTTP) ────────┘   │
                          │              │              │              │                                    │
                          │              ▼              ▼              ▼                                    │
                          │      ┌───────────┐   ┌───────────┐   ┌───────────┐                             │
                          │      │ postgres  │   │  redis    │   │  nats     │   ◀── NetworkPolicies:      │
                          │      │ (STS+PVC) │   │ (STS+PVC) │   │ (STS+PVC) │       default-deny ingress  │
                          │      │  :5432    │   │  :6379    │   │ :4222/:8222│      to data tier; only    │
                          │      └───────────┘   └───────────┘   └───────────┘       api/worker/migrate    │
                          │              │              │              │        pods may connect          │
                          └──────────────┼──────────────┼──────────────┼─────────────────────────────────┘
                                         ▼              ▼              ▼
                            PROD: use managed services (Terraform modules):
                            RDS/CloudSQL   ElastiCache/    self-managed NATS
                                           Memorystore     (modules/nats/README.md)
```

Flow summary: browsers hit the Ingress; `web` serves the SPA and proxies
`/api` (same-origin, `deploy/nginx.conf`); `api` serves `/api/v1` (real
`/healthz` + `/readyz`); `worker` consumes the Redis task queue
(`AGENTOS_QUEUE=redis`) and calls the model provider; status callbacks go
worker → api (`AGENTOS_API`, `X-API-Key`); events flow through NATS JetStream.

## Directory layout

```
deploy/
├── README.md                  <- this file
├── nginx.conf                 <- (pre-existing) web image nginx config
├── kubernetes/                <- canonical raw manifests (kubectl apply)
├── helm/agentos/              <- Helm chart (values, templates, NOTES.txt)
├── kustomize/                 <- base (synced copy) + dev/staging/production overlays
├── terraform/                 <- modules + environments (dev/staging/production)
└── validation/                <- offline validators used by CI (see below)
```

## Install paths

### 1) Raw kubectl (reference stack, in-cluster data tier)

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
# Secrets FIRST: render deploy/kubernetes/secret.template.yaml through
# sealed-secrets/external-secrets (placeholders only in git!).
kubectl apply -f deploy/kubernetes/            # everything (migrate Job included)
# Migrations are a Job; run once before the API starts and after upgrades:
kubectl -n agentos delete job agentos-migrate --ignore-not-found
kubectl -n agentos apply -f deploy/kubernetes/migrate-job.yaml
kubectl -n agentos wait --for=condition=complete job/agentos-migrate
kubectl -n agentos rollout status deploy/agentos-api deploy/agentos-worker deploy/agentos-web
```

### 2) Helm

```bash
helm upgrade --install agentos deploy/helm/agentos -n agentos --create-namespace \
  -f deploy/helm/agentos/values-dev.yaml        # or values-prod.yaml
helm test  # (none bundled) ; see NOTES.txt output after install
helm get notes agentos -n agentos
```

Key values knobs: `global.imageRegistry`, `api/worker/web.image.tag`,
`replicaCount`, `resources`, `autoscaling.{api,worker}`, `ingress.host/tls`,
`secrets.{create,existingSecret}`, `postgresql/redis/nats.deploy` (BYO
switches), `otel.sidecar.enabled` (OTLP sidecar — the app reads
`OTEL_EXPORTER_OTLP_ENDPOINT`, see `internal/observability/tracing.go`).

### 3) Kustomize

```bash
kubectl apply -k deploy/kustomize/overlays/dev          # ns agentos-dev
kubectl apply -k deploy/kustomize/overlays/staging      # ns agentos-staging
kubectl apply -k deploy/kustomize/overlays/production   # ns agentos-prod
```

Base is a **synced copy** of `deploy/kubernetes` (kubectl/kustomize refuse
`../` escapes by default). When you edit the canonical manifests, re-sync
`deploy/kustomize/base/` (`cp deploy/kubernetes/<files> deploy/kustomize/base/`)
— a CI diff check for drift is a worthwhile follow-up.

### 4) Terraform

```
deploy/terraform/
├── modules/
│   ├── agentos-api        # cloud-agnostic: k8s Deployment+Service+HPA+PDB
│   ├── agentos-worker     # cloud-agnostic: k8s Deployment+HPA+PDB (no Service/probes)
│   ├── aws/ecr            # image repositories (IMMUTABLE for prod)
│   ├── aws/irsa           # pod identity roles (ECR pull, scoped)
│   ├── database           # AWS RDS Postgres | GCP CloudSQL (flavor var)
│   ├── redis              # ElastiCache | Memorystore (flavor var)
│   └── nats/README.md     # guidance doc (self-managed NATS on k8s)
└── environments/{dev,staging,production}/   # root modules wiring the above
```

```bash
cd deploy/terraform/environments/dev
cp terraform.tfvars.example terraform.tfvars
terraform init && terraform plan && terraform apply
```

Status: **validated structurally (HCL sanity checks), plan-pending CI** — no
terraform binary is available in this workspace; CI must run `terraform
fmt -check`, `terraform validate`, and a reviewed plan before apply.
Note on `modules/database` / `modules/redis`: both include aws *and* google
provider blocks; with the count-based flavor switch only the selected branch's
resources are created, but `terraform init` downloads both providers.

## BYO database / cache / NATS (managed services in prod)

The in-cluster Postgres/Redis/NATS StatefulSets are **dev/small-stage
reference** deployments (single replica, simple PVCs, no backups — banners in
the manifests say so). For production:

1. Provision managed services with Terraform:
   `deploy/terraform/modules/database` (RDS/CloudSQL) and
   `modules/redis` (ElastiCache/Memorystore); NATS guidance in
   `modules/nats/README.md`.
2. Point the app at them:
   - **kubectl path**: edit the `agentos-config` ConfigMap
     (`REDIS_ADDR`, `AGENTOS_NATS_URL`) and the `agentos-secrets` Secret
     (`DATABASE_URL=postgres://user:pass@<endpoint>:5432/agentos?sslmode=require`);
     then remove the StatefulSets (`kubectl delete -f deploy/kubernetes/{postgres,redis,nats}-statefulset.yaml`).
   - **helm path**: `postgresql.deploy=false`, `redis.deploy=false` +
     `redis.externalAddr=<host>:6379`, `nats.deploy=false` +
     `nats.externalUrl=nats://...`, and set `secrets.databaseUrl`.
   - **terraform path**: the k8s modules read `config_map_name`/`secret_name`
     you pre-populate from the module outputs (`database.endpoint`,
     `redis.endpoint`).
3. NetworkPolicies: when the data tier leaves the namespace, drop the
   data-tier policies (`networkpolicy-data-*.yaml`) and scope egress to the
   managed endpoints (security groups / VPC routes handle it on the cloud side).

## Secret management

- `deploy/kubernetes/secret.template.yaml` and the chart's `secret.yaml` are
  **placeholders only** (`CHANGEME`). Never commit real values.
- Required keys (verified against `docker-compose.prod.yml` and the Go source):
  `DATABASE_URL`, `JWT_SECRET` (required in production, issue #55),
  `AGENTOS_SECRETS_MASTER_KEY` (required in Postgres mode, issue #25),
  `AGENTOS_API_KEY` (worker→api callbacks), `OPENAI_API_KEY` /
  `AGENTOS_FALLBACK_API_KEY` (optional — empty = offline mode),
  `POSTGRES_PASSWORD` (reference StatefulSet only).
- Recommended flow: **sealed-secrets** (commit SealedSecrets) or
  **external-secrets** (Vault/ASM/GSM backend). With the Helm chart set
  `secrets.create=false` + `secrets.existingSecret=<name>`; expected keys are
  listed in `NOTES.txt`. With the Terraform k8s modules pass your
  already-managed Secret via `secret_name`.
- RDS note: `modules/database` uses `manage_master_user_password` so the DB
  password never lands in tfstate.

## Upgrade / rollback procedure

Helm (recommended):

```bash
# Upgrade: migrations run as a pre-upgrade hook, then a zero-downtime rollout
# (maxSurge 1 / maxUnavailable 0 on api+web).
helm upgrade --install agentos deploy/helm/agentos -n agentos -f <values>
helm history agentos -n agentos

# Rollback (migrations are NOT auto-reverted; migrations/ are append-only --
# verify compatibility before rolling the app back across a schema change):
helm rollback agentos <revision> -n agentos
```

kubectl path: edit image tags / `kubectl set image`, watch
`kubectl rollout status`; rollback with `kubectl rollout undo deploy/agentos-api`.
Kustomize: bump the `images.newTag` in the overlay, `kubectl apply -k ...`,
`kubectl rollout undo` if needed.

Worker rollouts: queue work is re-delivered if a pod dies
(`AGENTOS_QUEUE=redis`), so `maxUnavailable: 1` is safe; give in-flight steps a
60s grace period (already set).

## Probe & HPA tuning notes

- **api**: `liveness=/healthz`, `readiness=/readyz`, startup allows ~2 min
  (24 × 5s) for boot + DB pool warmup. Tune after observing p99 `/readyz`
  latency; never point probes at anything else — those are the only health
  endpoints that exist.
- **worker**: no HTTP probes by design (no server). For queue-depth autoscaling
  install KEDA with a Redis list scaler on `REDIS_QUEUE_KEY` and disable the
  CPU HPA — documented in `modules/agentos-worker` outputs.
- **HPA api**: min 2 / max 10, CPU 70% + memory 80%; scaleDown window 300s
  (avoid flapping with long-lived dashboard connections).
- **HPA worker**: min 2 / max 20, CPU 70%; scaleUp doubles every 30s (queue
  bursts), scaleDown sheds 1 pod/min after a 600s window (drain long runs).
- **PDBs**: `minAvailable: 1` for api+worker (block disruptive drains below
  that); web has no PDB (stateless, cheap, surge rollouts).

## Validation (offline, no cluster required)

`deploy/validation/` contains the checks used for this deliverable — run them
in CI too:

```bash
python3 deploy/validation/validate_all.py       # runs everything below + secrets scan
python3 deploy/validation/validate_yaml.py     # every .yaml/.yml parses
python3 deploy/validation/validate_helm.py     # templates render-parse + values cross-check
python3 deploy/validation/validate_kustomize.py
python3 deploy/validation/validate_terraform.py # HCL structural sanity
```

Honest limitation: no `helm`/`kubectl`/`terraform` binaries in this workspace,
so templates are validated by (a) stripping `{{ ... }}` into placeholder
values and parsing the result as YAML, (b) cross-checking every
`.Values.*` key referenced by templates against values.yaml, and (c) structural
HCL checks (balanced braces/brackets, quoted strings, var cross-checks).
Full `helm lint`/`helm template`/`terraform validate` runs are the first CI job.
