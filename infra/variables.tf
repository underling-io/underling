# Each variable that reaches the container maps 1:1 onto the upper-case env
# var of the same name (docs/event-model.md § Configuration, § Bedrock mode).
# A null optional variable leaves its env var out, so the app's default holds.

variable "aws_region" {
  description = "AWS region for every resource, and the container's AWS_REGION (Claude Code's Bedrock region)."
  type        = string

  validation {
    condition     = can(regex("^[a-z]+(-[a-z]+)+-[0-9]+$", var.aws_region))
    error_message = "aws_region must be an AWS region: lowercase, hyphenated, ending in a digit (e.g. eu-central-1)."
  }
}

variable "domain_name" {
  description = "Fully qualified domain the ALB serves, e.g. underling.example.com. It must lie in hosted_zone_id."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]{2,}$", var.domain_name))
    error_message = "domain_name must be a lowercase fully qualified domain name without a trailing dot."
  }
}

variable "hosted_zone_id" {
  description = "ID of the existing public Route 53 hosted zone that holds domain_name."
  type        = string

  validation {
    condition     = can(regex("^Z[A-Z0-9]+$", var.hosted_zone_id))
    error_message = "hosted_zone_id must be a Route 53 hosted zone ID (e.g. Z0123456789ABCDEFGHIJ)."
  }
}

variable "image_tag" {
  description = "Tag of the underling image in the ECR repository, as pushed by make image-push. Tags are immutable."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$", var.image_tag))
    error_message = "image_tag must be a valid container image tag."
  }
}

variable "sentry_github_repos" {
  description = "SENTRY_GITHUB_REPOS: <projectId>=<owner>/<repo>, comma-separated."
  type        = string

  validation {
    condition     = trimspace(var.sentry_github_repos) != ""
    error_message = "sentry_github_repos must not be empty."
  }
}

variable "investigation_timeout" {
  description = "INVESTIGATION_TIMEOUT: a positive Go duration such as 30m. null keeps the app's default."
  type        = string
  default     = null

  validation {
    condition     = var.investigation_timeout == null || can(regex("^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$", var.investigation_timeout))
    error_message = "investigation_timeout must be a Go duration such as 30m or 1h30m."
  }
}

variable "anthropic_model" {
  description = "ANTHROPIC_MODEL: a Bedrock foundation-model or system-defined inference-profile id, e.g. global.anthropic.claude-sonnet-4-5-20250929-v1:0."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._:/-]+$", var.anthropic_model))
    error_message = "anthropic_model may contain only [A-Za-z0-9._:/-]."
  }
}

variable "anthropic_small_fast_model" {
  description = "ANTHROPIC_SMALL_FAST_MODEL: the model id for Claude Code's background tasks. null means the app's default, anthropic_model."
  type        = string
  default     = null

  validation {
    condition     = var.anthropic_small_fast_model == null || can(regex("^[A-Za-z0-9._:/-]+$", var.anthropic_small_fast_model))
    error_message = "anthropic_small_fast_model may contain only [A-Za-z0-9._:/-]."
  }
}

variable "codex_bedrock_model" {
  description = "CODEX_BEDROCK_MODEL: Codex's plain regional Bedrock (Mantle) model id, e.g. openai.gpt-oss-120b."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._:/-]+$", var.codex_bedrock_model))
    error_message = "codex_bedrock_model may contain only [A-Za-z0-9._:/-]."
  }
}

variable "codex_bedrock_region" {
  description = "CODEX_BEDROCK_REGION: the region of Codex's Bedrock endpoint. null means the app's default, aws_region."
  type        = string
  default     = null

  validation {
    condition     = var.codex_bedrock_region == null || can(regex("^[a-z]+(-[a-z]+)+-[0-9]+$", var.codex_bedrock_region))
    error_message = "codex_bedrock_region must be an AWS region: lowercase, hyphenated, ending in a digit."
  }
}

variable "task_cpu" {
  description = "Fargate task CPU units for the service."
  type        = number
  default     = 2048
}

variable "task_memory" {
  description = "Fargate task memory (MiB) for the service; must be valid for task_cpu."
  type        = number
  default     = 8192
}

variable "ephemeral_storage_gib" {
  description = "Fargate ephemeral storage (GiB) for the service; holds the event store and the per-run clones."
  type        = number
  default     = 30

  validation {
    condition     = var.ephemeral_storage_gib >= 21 && var.ephemeral_storage_gib <= 200
    error_message = "ephemeral_storage_gib must be between 21 and 200."
  }
}
