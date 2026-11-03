variable "name" {
  description = "Name of the cluster, the ECR repository and the container; prefix of the other names."
  type        = string
}

variable "aws_region" {
  description = "Region of the stack and of Claude Code's Bedrock calls."
  type        = string
}

variable "log_group_name" {
  description = "CloudWatch log group for the service and the probe."
  type        = string
}

variable "github_token_secret_name" {
  description = "Secrets Manager name of the GITHUB_TOKEN secret."
  type        = string
}

variable "sentry_client_secret_name" {
  description = "Secrets Manager name of the SENTRY_CLIENT_SECRET secret."
  type        = string
}

variable "image_tag" {
  description = "Image tag the probe task definition runs."
  type        = string
}

variable "anthropic_model_ids" {
  description = "Distinct Bedrock model or inference-profile ids Claude Code invokes."
  type        = list(string)
}

variable "codex_bedrock_model" {
  description = "Codex's Bedrock (Mantle) model id."
  type        = string
}

variable "codex_bedrock_region" {
  description = "Region of Codex's Bedrock (Mantle) endpoint."
  type        = string
}
