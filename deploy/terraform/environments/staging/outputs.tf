output "ecr_repositories" {
  description = "ECR repo URLs to build/push (api, worker, web)."
  value = {
    api    = module.ecr_api.repository_url
    worker = module.ecr_worker.repository_url
    web    = module.ecr_web.repository_url
  }
}

output "irsa_role_arn" {
  description = "IRSA role attached to the app ServiceAccounts."
  value       = module.irsa_app.role_arn
}

output "database_endpoint" {
  description = "RDS endpoint; build secrets.databaseUrl from it (sslmode=require)."
  value       = module.database.endpoint
}

output "database_password_hint" {
  description = "Where the RDS-managed master password lives."
  value       = module.database.password_secret_hint
}

output "redis_endpoint" {
  description = "ElastiCache endpoint; set REDIS_ADDR=<host>:6379."
  value       = "${module.redis.endpoint}:${module.redis.port}"
}

output "namespace" {
  description = "Namespace AgentOS was deployed into."
  value       = kubernetes_namespace.agentos.metadata[0].name
}
