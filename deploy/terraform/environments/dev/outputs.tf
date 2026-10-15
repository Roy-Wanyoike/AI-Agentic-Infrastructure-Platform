output "namespace" {
  description = "Namespace AgentOS was deployed into."
  value       = kubernetes_namespace.agentos.metadata[0].name
}

output "api_deployment" {
  description = "API deployment name."
  value       = module.agentos_api.deployment_name
}

output "worker_deployment" {
  description = "Worker deployment name."
  value       = module.agentos_worker.deployment_name
}

output "api_service" {
  description = "In-cluster API service (host:port for AGENTOS_API)."
  value       = "${module.agentos_api.service_name}:${module.agentos_api.service_port}"
}
