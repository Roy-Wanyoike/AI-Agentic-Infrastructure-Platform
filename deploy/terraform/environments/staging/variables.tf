variable "aws_region" {
  description = "AWS region for staging resources."
  type        = string
}

variable "cluster_endpoint" {
  description = "EKS API endpoint (kubernetes provider)."
  type        = string
}

variable "cluster_ca_cert" {
  description = "Base64 EKS cluster CA certificate."
  type        = string
  sensitive   = true
}

variable "cluster_token" {
  description = "Auth token for the kubernetes provider (CI: short-lived IRSA token)."
  type        = string
  sensitive   = true
}

variable "cluster_oidc_issuer" {
  description = "EKS OIDC issuer URL for IRSA (e.g. https://oidc.eks.<region>.amazonaws.com/id/<id>)."
  type        = string
}

variable "vpc_id" {
  description = "VPC hosting the EKS cluster."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet ids for RDS/ElastiCache (>= 2 AZs)."
  type        = list(string)
}

variable "cluster_node_security_group_id" {
  description = "Node security group allowed to reach the data tier."
  type        = string
}

variable "namespace" {
  description = "Namespace for the staging deployment."
  type        = string
  default     = "agentos"
}

variable "image_tag" {
  description = "Image tag pushed to the ECR repos (built by CI)."
  type        = string
  default     = "0.1.0"
}
