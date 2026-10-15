# AWS flavor: IRSA role for AgentOS pods (issue #117).
#
# PLACEHOLDER-by-design policy document: grants read-only ECR pull + optional
# S3/SSM reads so pods can pull images and read managed secrets. Scope it down
# to the specific repository ARNs / bucket ARNs in real environments.

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
  description = "IAM role name (e.g. agentos-api-irsa)."
  type        = string
}

variable "cluster_oidc_issuer" {
  description = "EKS cluster OIDC issuer URL, e.g. https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE (terraform output of the EKS module)."
  type        = string
}

variable "namespace" {
  description = "Kubernetes namespace whose ServiceAccounts may assume this role."
  type        = string
  default     = "agentos"
}

variable "service_accounts" {
  description = "ServiceAccount names allowed to assume this role (Federated subject)."
  type        = list(string)
  default     = ["agentos-api", "agentos-worker"]
}

variable "ecr_repository_arns" {
  description = "ECR repo ARNs the role may pull from (from modules/aws/ecr outputs)."
  type        = list(string)
  default     = ["*"]
}

variable "tags" {
  description = "Extra tags."
  type        = map(string)
  default     = {}
}

data "aws_iam_policy_document" "trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [replace(var.cluster_oidc_issuer, "https://", "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/")]
    }
    condition {
      test     = "StringEquals"
      variable = "${replace(var.cluster_oidc_issuer, "https://", "")}:sub"
      values   = [for sa in var.service_accounts : "system:serviceaccount:${var.namespace}:${sa}"]
    }
  }
}

data "aws_caller_identity" "current" {}

resource "aws_iam_role" "this" {
  name               = var.name
  assume_role_policy = data.aws_iam_policy_document.trust.json
  tags               = var.tags
}

data "aws_iam_policy_document" "permissions" {
  statement {
    sid       = "ECRPull"
    actions   = ["ecr:GetAuthorizationToken", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
    resources = ["*"]
  }
  statement {
    sid       = "ECRScope"
    actions   = ["ecr:DescribeImages", "ecr:BatchCheckLayerAvailability"]
    resources = var.ecr_repository_arns
  }
}

resource "aws_iam_role_policy" "this" {
  name   = "${var.name}-policy"
  role   = aws_iam_role.this.id
  policy = data.aws_iam_policy_document.permissions.json
}

output "role_arn" {
  description = "Pass into the k8s modules as service_account_annotations: { \"eks.amazonaws.com/role-arn\" = <this> }."
  value       = aws_iam_role.this.arn
}

output "role_name" {
  description = "IAM role name."
  value       = aws_iam_role.this.name
}
