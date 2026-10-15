# database module (issue #117): managed Postgres for AgentOS.
#
# Two honest, separately-enabled flavors selected by var.flavor:
#   flavor = "aws" -> RDS PostgreSQL 16 (publicly_not_accessible, encrypted,
#                     RDS-managed master password -- no secret in tfstate)
#   flavor = "gcp" -> Cloud SQL for PostgreSQL 16
# The flavors share variables/outputs; only one branch's resources are created.
# Plan review in CI is REQUIRED before apply (no terraform binary here).

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
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6"
    }
  }
}

variable "flavor" {
  description = "Which managed database flavor to create: \"aws\" (RDS) or \"gcp\" (Cloud SQL)."
  type        = string
  validation {
    condition     = contains(["aws", "gcp"], var.flavor)
    error_message = "flavor must be one of: aws, gcp."
  }
}

variable "name" {
  description = "Instance identifier (must be DNS-safe)."
  type        = string
  default     = "agentos-postgres"
}

# --- AWS flavor inputs ---
variable "aws_region" {
  description = "AWS region (flavor=aws)."
  type        = string
  default     = null
}

variable "aws_vpc_id" {
  description = "VPC id for the DB security group (flavor=aws)."
  type        = string
  default     = null
}

variable "aws_subnet_ids" {
  description = "Private subnet ids for the RDS subnet group (flavor=aws; needs >= 2 AZs)."
  type        = list(string)
  default     = []
}

variable "aws_allowed_security_group_ids" {
  description = "Security groups allowed to reach the DB (node/worker SGs of the cluster)."
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
  description = "VPC network id for private IP (flavor=gcp)."
  type        = string
  default     = null
}

# --- Shared inputs ---
variable "engine_version" {
  description = "Postgres major version (kept aligned with docker-compose.yml: 16)."
  type        = string
  default     = "16"
}

variable "instance_class" {
  description = "AWS: db.t4g.medium. GCP: db-custom-2-7680."
  type        = string
  default     = "db.t4g.medium"
}

variable "storage_gb" {
  description = "Allocated storage in GB."
  type        = number
  default     = 50
}

variable "database_name" {
  description = "Initial database name."
  type        = string
  default     = "agentos"
}

variable "username" {
  description = "Master/app username."
  type        = string
  default     = "agentos"
}

variable "multi_az" {
  description = "HA deployment (AWS Multi-AZ / GCP REGIONAL availability)."
  type        = bool
  default     = false
}

variable "backup_retention_days" {
  description = "Automated backup retention."
  type        = number
  default     = 7
}

variable "deletion_protection" {
  description = "Prevent accidental deletion (set true in production)."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Extra tags/labels."
  type        = map(string)
  default     = {}
}

# --------------------------------------------------------------------------
# AWS flavor (RDS)
# --------------------------------------------------------------------------
resource "aws_db_subnet_group" "this" {
  count = var.flavor == "aws" ? 1 : 0

  name       = "${var.name}-subnet-group"
  subnet_ids = var.aws_subnet_ids
  tags       = var.tags
}

resource "aws_security_group" "this" {
  count = var.flavor == "aws" ? 1 : 0

  name_prefix = "${var.name}-"
  vpc_id      = var.aws_vpc_id
  tags        = var.tags

  ingress {
    description     = "Postgres from allowed cluster security groups"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = var.aws_allowed_security_group_ids
  }

  egress {
    description = "Allow all egress (vpc-scoped)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_db_instance" "this" {
  count = var.flavor == "aws" ? 1 : 0

  identifier     = var.name
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  db_name  = var.database_name
  username = var.username
  # RDS-managed master password: never lands in tfstate; fetch via
  # rds-db / Secrets Manager at deploy time (see deploy/README.md).
  manage_master_user_password = true

  allocated_storage     = var.storage_gb
  max_allocated_storage = var.storage_gb * 4

  db_subnet_group_name   = aws_db_subnet_group.this[0].name
  vpc_security_group_ids = [aws_security_group.this[0].id]
  multi_az               = var.multi_az
  publicly_accessible    = false

  backup_retention_period = var.backup_retention_days
  deletion_protection     = var.deletion_protection
  storage_encrypted       = true

  skip_final_snapshot       = !var.deletion_protection
  final_snapshot_identifier = var.deletion_protection ? "${var.name}-final" : null

  tags = var.tags
}

# --------------------------------------------------------------------------
# GCP flavor (Cloud SQL)
# --------------------------------------------------------------------------
resource "random_password" "gcp_master" {
  count = var.flavor == "gcp" ? 1 : 0

  length  = 32
  special = true
}

resource "google_sql_database_instance" "this" {
  count = var.flavor == "gcp" ? 1 : 0

  name             = var.name
  project          = var.gcp_project
  region           = var.gcp_region
  database_version = "POSTGRES_${var.engine_version}"

  settings {
    tier                        = var.instance_class
    availability_type           = var.multi_az ? "REGIONAL" : "ZONAL"
    deletion_protection_enabled = var.deletion_protection
    disk_autoresize             = true
    disk_size                   = var.storage_gb

    backup_configuration {
      enabled                        = true
      start_time                     = "03:00"
      point_in_time_recovery_enabled = true
      backup_retention_settings {
        retained_backups = var.backup_retention_days
      }
    }

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.gcp_network_id
    }
  }

  deletion_protection = var.deletion_protection
}

resource "google_sql_database" "this" {
  count = var.flavor == "gcp" ? 1 : 0

  name     = var.database_name
  instance = google_sql_database_instance.this[0].name
  project  = var.gcp_project
}

resource "google_sql_user" "this" {
  count = var.flavor == "gcp" ? 1 : 0

  name     = var.username
  instance = google_sql_database_instance.this[0].name
  project  = var.gcp_project
  password = random_password.gcp_master[0].result
}

# --------------------------------------------------------------------------
# Outputs (shared shape across flavors so environments stay simple)
# --------------------------------------------------------------------------
output "endpoint" {
  description = "DB host:port to build the DSN from (secrets.databaseUrl)."
  value = var.flavor == "aws" ? (
    length(aws_db_instance.this) > 0 ? aws_db_instance.this[0].address : null
  ) : (
    length(google_sql_database_instance.this) > 0 ? google_sql_database_instance.this[0].private_ip_address : null
  )
}

output "port" {
  description = "DB port (5432 for both flavors)."
  value       = 5432
}

output "database_name" {
  description = "Database name."
  value       = var.database_name
}

output "username" {
  description = "Master username."
  value       = var.username
}

output "connection_name" {
  description = "GCP-only: Cloud SQL instance connection name (for the auth proxy). Null on AWS."
  value       = var.flavor == "gcp" && length(google_sql_database_instance.this) > 0 ? google_sql_database_instance.this[0].connection_name : null
}

output "flavor" {
  description = "Echoes the selected flavor (wiring sanity check)."
  value       = var.flavor
}

output "password_secret_hint" {
  description = "How to fetch the master password without it living in tfstate."
  value = var.flavor == "aws" ? "AWS: Secrets Manager entry managed by RDS (manage_master_user_password)" : "GCP: random_password.gcp_master in state -- migrate to Secret Manager before production use"
}
