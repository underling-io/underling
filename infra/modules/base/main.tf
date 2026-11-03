resource "aws_ecr_repository" "this" {
  name                 = var.name
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecs_cluster" "this" {
  name = var.name
}

resource "aws_cloudwatch_log_group" "this" {
  name              = var.log_group_name
  retention_in_days = 30
}

# Only the secrets, never a secret version: the values are set out of band
# (README.md) and never reach state.
resource "aws_secretsmanager_secret" "github_token" {
  name                    = var.github_token_secret_name
  description             = "heinzel GITHUB_TOKEN: pushes branches and opens PRs on the mapped repos; no workflow scope."
  recovery_window_in_days = 7
}

resource "aws_secretsmanager_secret" "sentry_client_secret" {
  name                    = var.sentry_client_secret_name
  description             = "heinzel SENTRY_CLIENT_SECRET: the Sentry internal integration's client secret."
  recovery_window_in_days = 7
}
