output "vpc_id" {
  value = aws_vpc.this.id
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "task_security_group_id" {
  value = aws_security_group.task.id
}

output "ecr_repository_url" {
  value = aws_ecr_repository.this.repository_url
}

output "cluster_arn" {
  value = aws_ecs_cluster.this.arn
}

output "cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.this.name
}

output "github_token_secret_arn" {
  value = aws_secretsmanager_secret.github_token.arn
}

output "sentry_client_secret_arn" {
  value = aws_secretsmanager_secret.sentry_client_secret.arn
}

# The role outputs wait for their policies, so a task never starts with a
# role that can't yet do its job.
output "execution_role_arn" {
  value      = aws_iam_role.execution.arn
  depends_on = [aws_iam_role_policy_attachment.execution, aws_iam_role_policy.execution_secrets]
}

output "task_role_arn" {
  value      = aws_iam_role.task.arn
  depends_on = [aws_iam_role_policy.task]
}

output "probe_task_definition_arn" {
  value = aws_ecs_task_definition.probe.arn
}
