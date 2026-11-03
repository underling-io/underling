locals {
  name                 = "heinzel"
  log_group_name       = "/ecs/heinzel"
  github_token_secret  = "heinzel/github-token"
  sentry_client_secret = "heinzel/sentry-client-secret"

  # The container env beyond what the image sets (docs/event-model.md
  # § Container image). Null values are left out.
  container_environment = {
    for name, value in {
      AWS_REGION                 = var.aws_region
      SENTRY_GITHUB_REPOS        = var.sentry_github_repos
      INVESTIGATION_TIMEOUT      = var.investigation_timeout
      ANTHROPIC_MODEL            = var.anthropic_model
      ANTHROPIC_SMALL_FAST_MODEL = var.anthropic_small_fast_model
      CODEX_BEDROCK_MODEL        = var.codex_bedrock_model
      CODEX_BEDROCK_REGION       = var.codex_bedrock_region
    } : name => value if value != null
  }
}

module "base" {
  source = "./modules/base"

  name                      = local.name
  aws_region                = var.aws_region
  log_group_name            = local.log_group_name
  github_token_secret_name  = local.github_token_secret
  sentry_client_secret_name = local.sentry_client_secret
  image_tag                 = var.image_tag

  # The two model ids Claude Code invokes by name: ANTHROPIC_MODEL and
  # ANTHROPIC_SMALL_FAST_MODEL, which defaults to it.
  anthropic_model_ids  = distinct([var.anthropic_model, coalesce(var.anthropic_small_fast_model, var.anthropic_model)])
  codex_bedrock_model  = var.codex_bedrock_model
  codex_bedrock_region = coalesce(var.codex_bedrock_region, var.aws_region)
}

module "service" {
  source = "./modules/service"

  name           = local.name
  aws_region     = var.aws_region
  domain_name    = var.domain_name
  hosted_zone_id = var.hosted_zone_id

  vpc_id                 = module.base.vpc_id
  public_subnet_ids      = module.base.public_subnet_ids
  task_security_group_id = module.base.task_security_group_id
  cluster_arn            = module.base.cluster_arn
  execution_role_arn     = module.base.execution_role_arn
  task_role_arn          = module.base.task_role_arn
  log_group_name         = module.base.log_group_name
  image                  = "${module.base.ecr_repository_url}:${var.image_tag}"

  task_cpu              = var.task_cpu
  task_memory           = var.task_memory
  ephemeral_storage_gib = var.ephemeral_storage_gib

  environment = local.container_environment
  secrets = {
    GITHUB_TOKEN         = module.base.github_token_secret_arn
    SENTRY_CLIENT_SECRET = module.base.sentry_client_secret_arn
  }
}
