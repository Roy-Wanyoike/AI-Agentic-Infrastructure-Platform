# redis module (issue #117): managed Redis for the AgentOS task queue.
#
# Two flavors selected by var.flavor (same honest pattern as modules/database):
#   flavor = "aws" -> ElastiCache replication group (cluster mode disabled),
#                     AOF/transit encryption per flags
#   flavor = "gcp" -> Memorystore for Redis
# The AgentOS queue uses a single Redis list (AGENTOS_QUEUE=redis,
# REDIS_QUEUE_KEY), so a single-shard replication group is the right size.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

variable "flavor" {
  description = "Which managed Redis flavor to create: \"aws\" (ElastiCache) or \"gcp\" (Memorystore)."
  type        = string
  validation {
    condition     = contains(["aws", "gcp"], var.flavor)
    error_message = "flavor must be one of: aws, gcp."
  }
}

variable "name" {
  description = "Cache identifier (DNS-safe)."
  type        = string
  default     = "agentos-redis"
}

# --- AWS flavor inputs ---
variable "aws_subnet_ids" {
  description = "Private subnet ids for the ElastiCache subnet group (flavor=aws)."
  type        = list(string)
  default     = []
}

variable "aws_allowed_security_group_ids" {
  description = "Security groups allowed to reach the cache (node SGs of the cluster)."
  type        = list(string)
  default     = []
}

# --- GCP flavor inputs ---
variable "gcp_project" {
  description = "GCP project id (flavor=gcp)."
  type        = string
  default     = null
}

variable "gcp_region" {
  description = "GCP region (flavor=gcp)."
  type        = string
  default     = null
}

variable "gcp_network_id" {
  description = "VPC network id for private services access (flavor=gcp)."
  type        = string
  default     = null
}

# --- Shared inputs ---
variable "engine_version" {
  description = "Redis engine version (aligned with docker-compose.yml: 7.x)."
  type        = string
  default     = "7.1"
}

variable "node_type" {
  description = "AWS: cache.t4g.small. GCP: memory size in GB (see memory_size_gb)."
  type        = string
  default     = "cache.t4g.small"
}

variable "memory_size_gb" {
  description = "GCP Memorystore memory size in GB (flavor=gcp)."
  type        = number
  default     = 1
}

variable "replicas_per_shard" {
  description = "AWS: automatic failover requires >= 1 replica. 0 = single node (dev)."
  type        = number
  default     = 1
}

variable "transit_encryption" {
  description = "TLS in transit. The AgentOS Redis client uses REDIS_ADDR; enable TLS and switch the address to rediss:// when you turn this on."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Extra tags/labels."
  type        = map(string)
  default     = {}
}

# --------------------------------------------------------------------------
# AWS flavor (ElastiCache)
# --------------------------------------------------------------------------
resource "aws_elasticache_subnet_group" "this" {
  count = var.flavor == "aws" ? 1 : 0

  name       = "${var.name}-subnet-group"
  subnet_ids = var.aws_subnet_ids
}

resource "aws_security_group" "this" {
  count = var.flavor == "aws" ? 1 : 0

  name_prefix = "${var.name}-"
  description = "AgentOS Redis access"
  tags        = var.tags

  ingress {
    description     = "Redis from allowed cluster security groups"
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = var.aws_allowed_security_group_ids
  }
}

resource "aws_elasticache_replication_group" "this" {
  count = var.flavor == "aws" ? 1 : 0

  replication_group_id = var.name
  description          = "AgentOS task queue cache"
  engine               = "redis"
  engine_version       = var.engine_version
  node_type            = var.node_type

  num_cache_clusters         = var.replicas_per_shard + 1
  automatic_failover_enabled = var.replicas_per_shard > 0
  multi_az_enabled           = var.replicas_per_shard > 0

  subnet_group_name  = aws_elasticache_subnet_group.this[0].name
  security_group_ids = [aws_security_group.this[0].id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = var.transit_encryption

  snapshot_retention_limit = 5
  snapshot_window          = "03:00-05:00"

  tags = var.tags
}

# --------------------------------------------------------------------------
# GCP flavor (Memorystore)
# --------------------------------------------------------------------------
resource "google_redis_instance" "this" {
  count = var.flavor == "gcp" ? 1 : 0

  name           = var.name
  project        = var.gcp_project
  region         = var.gcp_region
  tier           = var.replicas_per_shard > 0 ? "STANDARD_HA" : "BASIC"
  memory_size_gb = var.memory_size_gb
  redis_version  = "REDIS_7_0"

  authorized_network = var.gcp_network_id
  connect_mode       = "PRIVATE_SERVICE_ACCESS"

  redis_configs = {
    maxmemory-policy = "noeviction" # never silently drop queued tasks
  }
}

# --------------------------------------------------------------------------
# Outputs
# --------------------------------------------------------------------------
output "endpoint" {
  description = "Cache host to set as REDIS_ADDR (<host>:6379)."
  value = var.flavor == "aws" ? (
    length(aws_elasticache_replication_group.this) > 0 ? aws_elasticache_replication_group.this[0].primary_endpoint_address : null
  ) : (
    length(google_redis_instance.this) > 0 ? google_redis_instance.this[0].host : null
  )
}

output "port" {
  description = "Cache port (6379)."
  value       = 6379
}

output "tls" {
  description = "Whether transit encryption is on (switch REDIS_ADDR to rediss:// when true)."
  value       = var.transit_encryption
}

output "flavor" {
  description = "Echoes the selected flavor (wiring sanity check)."
  value       = var.flavor
}
