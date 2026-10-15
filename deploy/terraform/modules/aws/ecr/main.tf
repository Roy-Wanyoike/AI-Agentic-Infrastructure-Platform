# AWS flavor: ECR repositories for the agentos images (issue #117).
# Used to build/push agentos-api / agentos-worker / agentos-web before
# pointing the k8s modules at the resulting <repo_url>:<tag>.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "name" {
  description = "Repository name (e.g. agentos-api, agentos-worker, agentos-web)."
  type        = string
}

variable "image_tag_mutability" {
  description = "MUTABLE for dev convenience, IMMUTABLE for prod (recommended)."
  type        = string
  default     = "IMMUTABLE"
}

variable "scan_on_push" {
  description = "Enable ECR basic scan on push."
  type        = bool
  default     = true
}

variable "max_image_count" {
  description = "Lifecycle cap on retained images (0 = no lifecycle policy)."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Extra tags for the repository."
  type        = map(string)
  default     = {}
}

resource "aws_ecr_repository" "this" {
  name                 = var.name
  image_tag_mutability = var.image_tag_mutability
  force_delete         = false

  image_scanning_configuration {
    scan_on_push = var.scan_on_push
  }

  tags = var.tags
}

resource "aws_ecr_lifecycle_policy" "this" {
  count = var.max_image_count > 0 ? 1 : 0
  repository = aws_ecr_repository.this.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Keep last ${var.max_image_count} images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = var.max_image_count
        }
        action = {
          type = "expire"
        }
      }
    ]
  })
}

output "repository_url" {
  description = "ECR repository URL; append :<tag> and pass as the k8s module image."
  value       = aws_ecr_repository.this.repository_url
}

output "repository_arn" {
  description = "Repository ARN (scope IRSA/CI pull policies to this)."
  value       = aws_ecr_repository.this.arn
}

output "registry_id" {
  description = "AWS account id hosting the repository (ECR login target)."
  value       = aws_ecr_repository.this.registry_id
}
