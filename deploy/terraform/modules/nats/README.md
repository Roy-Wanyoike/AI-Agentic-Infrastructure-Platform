# NATS module (issue #117) -- GUIDANCE ONLY (no Terraform resources).
#
# There is no first-class managed NATS on AWS/GCP, and the AgentOS event
# publisher + webhook delivery need JetStream. Recommended production path:
# self-manage NATS on Kubernetes with the official chart; keep this module as
# documentation and wire outputs of that install into AgentOS config.
#
# Recommended setup (nats-server helm chart, https://github.com/nats-io/k8s):
#
#   helm repo add nats https://nats-io.github.io/k8s/helm/charts/
#   helm upgrade --install nats nats/nats -n events --create-namespace \
#     --set cluster.replicas=3 \                      # JetStream Raft: 3 nodes
#     --set jetstream.enabled=true \
#     --set jetstream.fileStorage.size=10Gi \
#     --set jetstream.fileStorage.storageClassName=fast-ssd \  # low-latency NVMe
#     --set mesh.replicas=3 \                         # leaf/monitor as needed
#     --set resources.requests.cpu=250m \
#     --set resources.requests.memory=256Mi
#
# Operational guidance:
#   * 3+ replicas for JetStream Raft quorum; spread across AZs
#     (topologySpreadConstraints / anti-affinity).
#   * Pin a fast StorageClass (SSD/NVMe): JetStream is latency-sensitive;
#     network-attached HDDs will throttle stream publishes.
#   * Backups: `nats stream backup`/`restore` or snapshot the PVCs; JetStream
#     is NOT a drop-in durable store -- test restores.
#   * Monitoring: the server exposes /healthz on the monitor port (8222) --
#     probe THAT endpoint (it is real; docker-compose.prod.yml uses it too),
#     and scrape /varz, /connz, /jsz with Prometheus.
#   * Resource hints: start at 250m/256Mi per node, alert on
#     memory use relative to jetstream limits.
#   * Security: run without accounts first for bring-up, then enable
#     accounts/NKEYs or an operator with JWTs before exposing beyond the
#     cluster namespace. Restrict ingress with a NetworkPolicy to api/worker pods.
#
# Wiring into AgentOS:
#   1. Deploy the chart (above) into a namespace reachable by api/worker pods.
#   2. Set the app env (helm values config or the agentos ConfigMap):
#        AGENTOS_NATS_URL=nats://nats.events.svc.cluster.local:4222
#      (in the Helm chart: nats.deploy=false, nats.externalUrl=<url>).
#   3. NetworkPolicy: allow egress api/worker -> NATS pods on 4222 (see
#      deploy/kubernetes/networkpolicy-*.yaml for the pattern to copy).
#   4. If you later adopt NATS managed offerings (Synadia Cloud / self-hosted
#      leaf nodes), only the URL changes -- no AgentOS code changes.
