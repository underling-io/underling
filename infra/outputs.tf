# Every output resolves after `tofu apply -exclude=module.service`: none reads
# module.service. 016.04 and README.md rely on these names.

output "base_url" {
  description = "Public base URL of heinzel."
  value       = "https://${var.domain_name}"
}

output "webhook_url" {
  description = "The Sentry integration's webhook URL."
  value       = "https://${var.domain_name}/sentry/callback"
}

output "ecr_repository_url" {
  description = "ECR repository URL; pass it to make image-push as ECR_REPOSITORY_URL."
  value       = module.base.ecr_repository_url
}

output "log_group_name" {
  description = "CloudWatch log group of the service (stream prefix heinzel/) and the probe (probe/)."
  value       = module.base.log_group_name
}

output "github_token_secret_arn" {
  description = "Secrets Manager secret holding GITHUB_TOKEN. OpenTofu never sets its value."
  value       = module.base.github_token_secret_arn
}

output "sentry_client_secret_arn" {
  description = "Secrets Manager secret holding SENTRY_CLIENT_SECRET. OpenTofu never sets its value."
  value       = module.base.sentry_client_secret_arn
}

output "cluster_name" {
  description = "ECS cluster name."
  value       = module.base.cluster_name
}

output "service_name" {
  description = "ECS service name."
  value       = local.name
}

output "task_role_arn" {
  description = "IAM role the heinzel task runs as (Bedrock access)."
  value       = module.base.task_role_arn
}

output "probe_task_definition_arn" {
  description = "Task definition that runs heinzel-sandbox-probe as a one-off Fargate task."
  value       = module.base.probe_task_definition_arn
}

output "public_subnet_ids" {
  description = "The two public subnets."
  value       = module.base.public_subnet_ids
}

output "task_security_group_id" {
  description = "Security group of the heinzel tasks."
  value       = module.base.task_security_group_id
}

output "probe_network_configuration" {
  description = "Ready-made --network-configuration for aws ecs run-task of the probe."
  value = jsonencode({
    awsvpcConfiguration = {
      subnets        = module.base.public_subnet_ids
      securityGroups = [module.base.task_security_group_id]
      assignPublicIp = "ENABLED"
    }
  })
}
