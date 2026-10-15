# agentos-api module (issue #117) -- cloud-agnostic core.
#
# Deploys the AgentOS API (Deployment + ClusterIP Service + CPU HPA + PDB) via
# the `kubernetes` provider, which works against EKS, GKE, AKS, and kind alike.
# Probes target the REAL cmd/api endpoints /healthz + /readyz.
#
# Cloud-specific wiring stays in flavor modules:
#   * AWS: modules/aws/ecr (image repo) + modules/aws/irsa (pod identity) --
#     pass their outputs in as `image` and `service_account_annotations`.
# No provider conditionals here: one honest code path per concern.

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
  description = "Base name for API resources (e.g. agentos-api)."
  type        = string
  default     = "agentos-api"
}

variable "namespace" {
  description = "Kubernetes namespace to deploy into (must exist)."
  type        = string
}

variable "image" {
  description = "API container image, e.g. ghcr.io/roy-wanyoike/agentos-api:0.1.0 (or your ECR/GAR mirror)."
  type        = string
}

variable "replicas" {
  description = "Baseline replica count; the HPA overrides it when enabled."
  type        = number
  default     = 2
}

variable "port" {
  description = "API container port (cmd/api listens on API_PORT, default 8080)."
  type        = number
  default     = 8080
}

variable "service_account_name" {
  description = "ServiceAccount for the API pods."
  type        = string
  default     = "agentos-api"
}

variable "service_account_annotations" {
  description = "ServiceAccount annotations; for AWS IRSA pass { \"eks.amazonaws.com/role-arn\" = <role_arn> }."
  type        = map(string)
  default     = {}
}

variable "config_map_name" {
  description = "ConfigMap with non-secret env (rendered separately or via helm/kustomize)."
  type        = string
  default     = "agentos-config"
}

variable "secret_name" {
  description = "Secret holding DATABASE_URL / JWT_SECRET / AGENTOS_SECRETS_MASTER_KEY (create it with sealed-secrets/external-secrets -- never Terraform plaintext)."
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
    JWT_SECRET                 = { key = "jwtSecret" }
    AGENTOS_SECRETS_MASTER_KEY = { key = "secretsMasterKey" }
    OPENAI_API_KEY             = { key = "openaiApiKey", optional = true }
  }
}

variable "resources" {
  description = "Container resources for the API."
  type = object({
    requests = optional(map(string), { cpu = "100m", memory = "128Mi" })
    limits   = optional(map(string), { memory = "512Mi" })
  })
  default = {}
}

variable "hpa" {
  description = "CPU-based HPA (autoscaling/v1 in the provider; memory targets need a kubernetes_manifest -- see README)."
  type = object({
    enabled                = optional(bool, true)
    min_replicas           = optional(number, 2)
    max_replicas           = optional(number, 10)
    target_cpu_utilization = optional(number, 70)
  })
  default = {}
}

variable "pdb_min_available" {
  description = "PDB minAvailable; set 0 to skip the PDB."
  type        = number
  default     = 1
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
      "app.kubernetes.io/component" = "api"
    },
    var.labels
  )
}

resource "kubernetes_service_account" "api" {
  metadata {
    name        = var.service_account_name
    namespace   = var.namespace
    labels      = local.common
    annotations = var.service_account_annotations
  }
  automount_service_account_token = false
}

resource "kubernetes_deployment" "api" {
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
        max_unavailable = "0"
      }
    }

    template {
      metadata {
        labels = local.common
      }
      spec {
        service_account_name             = kubernetes_service_account.api.metadata[0].name
        automount_service_account_token  = false
        termination_grace_period_seconds = 30

        security_context {
          run_as_non_root = true
          run_as_user     = 65532 # distroless nonroot uid (Dockerfile.api)
          run_as_group    = 65532
          fs_group        = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = "api"
          image             = var.image
          image_pull_policy = "IfNotPresent"

          port {
            name           = "http"
            container_port = var.port
          }

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

          # REAL endpoints from cmd/api/main.go (routes()).
          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            period_seconds        = 10
            timeout_seconds       = 2
            failure_threshold     = 6
            initial_delay_seconds = 5
          }
          readiness_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            period_seconds    = 5
            timeout_seconds   = 2
            failure_threshold = 6
          }
          startup_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            period_seconds    = 5
            timeout_seconds   = 2
            failure_threshold = 24
          }

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

resource "kubernetes_service" "api" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.common
  }
  spec {
    type     = "ClusterIP"
    selector = local.common
    port {
      name        = "http"
      port        = var.port
      target_port = "http"
    }
  }
}

resource "kubernetes_horizontal_pod_autoscaler" "api" {
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

resource "kubernetes_pod_disruption_budget_v1" "api" {
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
  description = "Name of the API Deployment."
  value       = kubernetes_deployment.api.metadata[0].name
}

output "service_name" {
  description = "Name of the API ClusterIP Service (in-cluster: http://<service_name>:<port>)."
  value       = kubernetes_service.api.metadata[0].name
}

output "service_port" {
  description = "Port the API Service exposes."
  value       = var.port
}

output "service_account_name" {
  description = "ServiceAccount the API pods run as (attach IRSA role here on AWS)."
  value       = kubernetes_service_account.api.metadata[0].name
}

output "image" {
  description = "Image deployed (echoes the resolved input)."
  value       = var.image
}
