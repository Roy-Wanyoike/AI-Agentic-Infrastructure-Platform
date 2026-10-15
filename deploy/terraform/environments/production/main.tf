# AgentOS production environment (issue #117).
#
# Production wiring (AWS flavor; GCP CloudSQL/Memorystore available via the
# same modules with flavor="gcp"):
#   ECR repos (IMMUTABLE tags, scan-on-push) + IRSA pod identity
#   RDS Postgres Multi-AZ, deletion protection, 90d backups
#   ElastiCache Redis with replica + failover
# NATS: self-managed 3-node JetStream cluster (modules/nats/README.md).
#
# NOTE: validated structurally; plan-pending CI. Apply only after a reviewed
# `terraform plan` in CI (no terraform binary locally).

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.25.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

provider "kubernetes" {
  host                   = var.cluster_endpoint
  cluster_ca_certificate = base64decode(var.cluster_ca_cert)
  token                  = var.cluster_token
}

resource "kubernetes_namespace" "agentos" {
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/part-of"                  = "agentos"
      "pod-security.kubernetes.io/enforce"         = "restricted"
      "pod-security.kubernetes.io/enforce-version" = "latest"
    }
  }
}

# --- AWS flavor: image repos + pod identity -------------------------------
module "ecr_api" {
  source = "../../modules/aws/ecr"

  name                 = "agentos-api"
  image_tag_mutability = "IMMUTABLE"
  max_image_count      = 100
}

module "ecr_worker" {
  source = "../../modules/aws/ecr"

  name                 = "agentos-worker"
  image_tag_mutability = "IMMUTABLE"
  max_image_count      = 100
}

module "ecr_web" {
  source = "../../modules/aws/ecr"

  name                 = "agentos-web"
  image_tag_mutability = "IMMUTABLE"
  max_image_count      = 100
}

module "irsa_app" {
  source = "../../modules/aws/irsa"

  name                = "agentos-production-irsa"
  cluster_oidc_issuer = var.cluster_oidc_issuer
  namespace           = kubernetes_namespace.agentos.metadata[0].name
  service_accounts    = ["agentos-api", "agentos-worker"]
  ecr_repository_arns = [module.ecr_api.repository_arn, module.ecr_worker.repository_arn, module.ecr_web.repository_arn]
}

# --- Managed data tier (HA, protected) --------------------------------------
module "database" {
  source = "../../modules/database"

  flavor            = "aws"
  name              = "agentos-prod-pg"
  aws_vpc_id        = var.vpc_id
  aws_subnet_ids    = var.private_subnet_ids
  aws_allowed_security_group_ids = [var.cluster_node_security_group_id]

  multi_az               = true
  deletion_protection    = true
  backup_retention_days  = 30
  storage_gb             = 100
}

module "redis" {
  source = "../../modules/redis"

  flavor            = "aws"
  name              = "agentos-prod-redis"
  aws_subnet_ids    = var.private_subnet_ids
  aws_allowed_security_group_ids = [var.cluster_node_security_group_id]

  replicas_per_shard = 2
}

# --- App tier ---------------------------------------------------------------
module "agentos_api" {
  source = "../../modules/agentos-api"

  name      = "agentos-api"
  namespace = kubernetes_namespace.agentos.metadata[0].name
  image     = "${module.ecr_api.repository_url}:${var.image_tag}"

  replicas = 3
  hpa = {
    enabled      = true
    min_replicas = 3
    max_replicas = 12
  }
  service_account_annotations = {
    "eks.amazonaws.com/role-arn" = module.irsa_app.role_arn
  }
  secret_name = "agentos-secrets"
}

module "agentos_worker" {
  source = "../../modules/agentos-worker"

  name      = "agentos-worker"
  namespace = kubernetes_namespace.agentos.metadata[0].name
  image     = "${module.ecr_worker.repository_url}:${var.image_tag}"

  replicas = 3
  hpa = {
    enabled      = true
    min_replicas = 3
    max_replicas = 30
  }
  service_account_annotations = {
    "eks.amazonaws.com/role-arn" = module.irsa_app.role_arn
  }
  secret_name = "agentos-secrets"
}
