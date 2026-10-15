variable "kubeconfig_path" {
  description = "Path to the kubeconfig for the dev cluster."
  type        = string
  default     = "~/.kube/config"
}

variable "kubeconfig_context" {
  description = "Kubeconfig context to use (e.g. k3d-agentos-dev)."
  type        = string
  default     = null
}

variable "namespace" {
  description = "Namespace for the dev deployment."
  type        = string
  default     = "agentos"
}

variable "image_tag" {
  description = "Image tag for the ghcr.io/roy-wanyoike/agentos-* images."
  type        = string
  default     = "0.1.0"
}
