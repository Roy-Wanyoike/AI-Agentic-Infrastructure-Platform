# AgentOS dev environment (issue #117).
#
# Dev wiring: NO managed data tier -- the in-cluster reference StatefulSets
# from deploy/kubernetes (or the helm chart with postgresql/redis/nats
# .deploy=true) provide Postgres/Redis/NATS. This root module only deploys the
# API + worker k8s modules with small footprints.
#
# NOTE: terraform binary availability -- this file is validated structurally
# (see deploy/validation/README) and is plan-pending CI.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.25.0"
    }
  }
}

provider "kubernetes" {
  # Point at your dev cluster (kind/k3d/minikube or a remote dev cluster).
  config_path            = var.kubeconfig_path
  config_context         = var.kubeconfig_context
}

locals {
  image_tag = var.image_tag
}

resource "kubernetes_namespace" "agentos" {
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/part-of"               = "agentos"
      "pod-security.kubernetes.io/enforce"      = "restricted"
      "pod-security.kubernetes.io/enforce-version" = "latest"
    }
  }
}

module "agentos_api" {
  source = "../../modules/agentos-api"

  name      = "agentos-api"
  namespace = kubernetes_namespace.agentos.metadata[0].name
  image     = "ghcr.io/roy-wanyoike/agentos-api:${local.image_tag}"

  replicas = 1
  hpa = {
    enabled      = false
    min_replicas = 1
    max_replicas = 2
  }
  pdb_min_available = 0 # single replica in dev: a PDB would block drains

  secret_name = "agentos-secrets" # created out-of-band for dev (see README)
}

module "agentos_worker" {
  source = "../../modules/agentos-worker"

  name      = "agentos-worker"
  namespace = kubernetes_namespace.agentos.metadata[0].name
  image     = "ghcr.io/roy-wanyoike/agentos-worker:${local.image_tag}"

  replicas = 1
  hpa = {
    enabled      = false
    min_replicas = 1
    max_replicas = 2
  }
  pdb_min_available = 0

  secret_name = "agentos-secrets"
}
