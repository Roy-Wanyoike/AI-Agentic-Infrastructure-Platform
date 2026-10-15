# agentos-worker module (issue #117) -- cloud-agnostic core.
#
# Deploys the AgentOS worker fleet (Deployment + CPU HPA + PDB) via the
# `kubernetes` provider. HONEST NOTE mirrored from the manifests: the worker
# runs NO HTTP server (Dockerfile.worker), so this module deliberately renders
# NO probes and NO Service. Liveness is observed via logs/events; the queue
# re-delivers work if a pod dies.
#
# Status callbacks go back to the API over AGENTOS_API (+ AGENTOS_API_KEY).

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.25.0"
    }
  }
}

variable "name" {
  description = "Base name for worker resources (e.g. agentos-worker)."
  type        = string
  default     = "agentos-worker"
}

variable "namespace" {
  description = "Kubernetes namespace to deploy into (must exist)."
  type        = string
}

variable "image" {
  description = "Worker container image, e.g. ghcr.io/roy-wanyoike/agentos-worker:0.1.0 (or your ECR/GAR mirror)."
  type        = string
}

variable "replicas" {
  description = "Baseline replica count; the HPA overrides it when enabled."
  type        = number
  default     = 2
}

variable "service_account_name" {
  description = "ServiceAccount for the worker pods."
  type        = string
  default     = "agentos-worker"
}

variable "service_account_annotations" {
  description = "ServiceAccount annotations; for AWS IRSA pass { \"eks.amazonaws.com/role-arn\" = <role_arn> }."
  type        = map(string)
  default     = {}
}

variable "config_map_name" {
  description = "ConfigMap with non-secret env incl. AGENTOS_API (worker -> API base URL)."
  type        = string
  default     = "agentos-config"
}

variable "secret_name" {
  description = "Secret holding DATABASE_URL / callback key / model provider keys."
  type        = string
  default     = "agentos-secrets"
}

variable "secret_keys" {
  description = "Mapping of env var name -> secret key inside var.secret_name."
  type = map(object({
    key      = string
    optional = optional(bool, false)
  }))
  default = {
    DATABASE_URL               = { key = "databaseUrl" }
    AGENTOS_API_KEY            = { key = "apiKey", optional = true }
    OPENAI_API_KEY             = { key = "openaiApiKey", optional = true }
    AGENTOS_FALLBACK_API_KEY   = { key = "fallbackApiKey", optional = true }
  }
}

variable "resources" {
  description = "Container resources for the worker."
  type = object({
    requests = optional(map(string), { cpu = "100m", memory = "128Mi" })
    limits   = optional(map(string), { memory = "1Gi" })
  })
  default = {}
}

variable "hpa" {
  description = "CPU-based HPA. For queue-depth scaling see KEDA guidance in deploy/README.md."
  type = object({
    enabled                = optional(bool, true)
    min_replicas           = optional(number, 2)
    max_replicas           = optional(number, 20)
    target_cpu_utilization = optional(number, 70)
  })
  default = {}
}

variable "pdb_min_available" {
  description = "PDB minAvailable; set 0 to skip the PDB."
  type        = number
  default     = 1
}

variable "termination_grace_period_seconds" {
  description = "Grace period so in-flight run steps can finish before SIGKILL."
  type        = number
  default     = 60
}

variable "labels" {
  description = "Extra labels merged onto all resources."
  type        = map(string)
  default     = {}
}

locals {
  common = merge(
    {
      "app.kubernetes.io/part-of"   = "agentos"
      "app.kubernetes.io/name"      = var.name
      "app.kubernetes.io/component" = "worker"
    },
    var.labels
  )
}

resource "kubernetes_service_account" "worker" {
  metadata {
    name        = var.service_account_name
    namespace   = var.namespace
    labels      = local.common
    annotations = var.service_account_annotations
  }
  automount_service_account_token = false
}

resource "kubernetes_deployment" "worker" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.common
  }

  spec {
    replicas = var.replicas
    selector {
      match_labels = local.common
    }
    strategy {
      type = "RollingUpdate"
      rolling_update {
        max_surge       = "1"
        max_unavailable = "1"
      }
    }

    template {
      metadata {
        labels = local.common
      }
      spec {
        service_account_name            = kubernetes_service_account.worker.metadata[0].name
        automount_service_account_token = false
        termination_grace_period_seconds = var.termination_grace_period_seconds

        security_context {
          run_as_non_root = true
          run_as_user     = 65532
          run_as_group    = 65532
          fs_group        = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = "worker"
          image             = var.image
          image_pull_policy = "IfNotPresent"

          env_from {
            config_map_ref {
              name = var.config_map_name
            }
          }

          dynamic "env" {
            for_each = var.secret_keys
            content {
              name  = env.key
              value_from {
                secret_key_ref {
                  name     = var.secret_name
                  key      = env.value.key
                  optional = env.value.optional
                }
              }
            }
          }

          # Deliberately NO liveness/readiness/startup probes: the worker runs
          # no HTTP server (Dockerfile.worker, cmd/worker/main.go). Adding an
          # httpGet probe here would be inventing an endpoint that doesn't exist.

          resources {
            requests = var.resources.requests
            limits   = var.resources.limits
          }

          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }

          volume_mount {
            name       = "tmp"
            mount_path = "/tmp"
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }
      }
    }
  }
}

resource "kubernetes_horizontal_pod_autoscaler" "worker" {
  count = var.hpa.enabled ? 1 : 0
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.common
  }
  spec {
    min_replicas                      = var.hpa.min_replicas
    max_replicas                      = var.hpa.max_replicas
    target_cpu_utilization_percentage = var.hpa.target_cpu_utilization
    scale_target_ref {
      api_version = "apps/v1"
      kind        = "Deployment"
      name        = var.name
    }
  }
}

resource "kubernetes_pod_disruption_budget_v1" "worker" {
  count = var.pdb_min_available > 0 ? 1 : 0
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.common
  }
  spec {
    min_available = var.pdb_min_available
    selector {
      match_labels = local.common
    }
  }
}

output "deployment_name" {
  description = "Name of the worker Deployment."
  value       = kubernetes_deployment.worker.metadata[0].name
}

output "service_account_name" {
  description = "ServiceAccount the worker pods run as (attach IRSA role here on AWS)."
  value       = kubernetes_service_account.worker.metadata[0].name
}

output "image" {
  description = "Image deployed (echoes the resolved input)."
  value       = var.image
}

output "notes" {
  description = "Operational notes (no probes by design)."
  value       = "Worker exposes no HTTP endpoints; no probes/Service rendered on purpose (Dockerfile.worker). Scale on queue depth with KEDA for tighter backlog control."
}
