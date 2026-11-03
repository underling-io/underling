variable "name" {
  description = "Name of the service, the task family, the container and the ALB."
  type        = string
}

variable "aws_region" {
  description = "Region of the stack (for the awslogs driver)."
  type        = string
}

variable "domain_name" {
  description = "Domain the ALB serves."
  type        = string
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone that holds domain_name."
  type        = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  type = list(string)
}

variable "task_security_group_id" {
  type = string
}

variable "cluster_arn" {
  type = string
}

variable "execution_role_arn" {
  type = string
}

variable "task_role_arn" {
  type = string
}

variable "log_group_name" {
  type = string
}

variable "image" {
  description = "Full image reference, <ecr url>:<tag>."
  type        = string
}

variable "task_cpu" {
  type = number
}

variable "task_memory" {
  type = number
}

variable "ephemeral_storage_gib" {
  type = number
}

variable "environment" {
  description = "Container env vars, name => value."
  type        = map(string)
}

variable "secrets" {
  description = "Container env vars from Secrets Manager, name => secret ARN."
  type        = map(string)
}
