variable "region" {
  description = "AWS region to deploy into"
  default     = "eu-north-1"
}

variable "cluster_name" {
  description = "EKS cluster name"
  default     = "di-agent-eks"
}

variable "kubernetes_version" {
  description = "Kubernetes version for the EKS control plane; null uses the EKS default"
  type        = string
  default     = null
}

variable "node_count" {
  description = "Desired size of the managed node group (mirrors the local PoC's worker count)"
  default     = 3
}

variable "instance_type" {
  description = "Instance type for the managed node group"
  default     = "t3.medium"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC created for this cluster"
  default     = "10.60.0.0/16"
}

variable "api_access_cidrs" {
  description = "IPv4 CIDRs allowed to reach the public Kubernetes API endpoint"
  type        = list(string)

  validation {
    condition     = length(var.api_access_cidrs) > 0 && alltrue([for cidr in var.api_access_cidrs : can(cidrnetmask(cidr)) && !endswith(cidr, "/0")])
    error_message = "Provide at least one valid IPv4 CIDR, restricted to operator or VPN addresses."
  }
}
