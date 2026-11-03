data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  account   = data.aws_caller_identity.current.account_id
  partition = data.aws_partition.current.partition
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# Execution role: pulls the image, writes logs, reads the two secrets.

resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "execution_secrets" {
  statement {
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      aws_secretsmanager_secret.github_token.arn,
      aws_secretsmanager_secret.sentry_client_secret.arn,
    ]
  }
}

resource "aws_iam_role_policy" "execution_secrets" {
  name   = "secrets"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution_secrets.json
}

# Task role: exactly docs/event-model.md § AWS needs, nothing else (no ECS
# Exec).

data "aws_iam_policy_document" "task_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${local.partition}:ecs:${var.aws_region}:${local.account}:*"]
    }
  }
}

resource "aws_iam_role" "task" {
  name               = "${var.name}-task"
  assume_role_policy = data.aws_iam_policy_document.task_assume.json
}

# Classify each model id against what Bedrock offers in the region. Both
# lookups run at plan time, so the operator needs bedrock:ListInferenceProfiles
# and bedrock:ListFoundationModels.
data "aws_bedrock_inference_profiles" "system" {
  type = "SYSTEM_DEFINED"
}

data "aws_bedrock_foundation_models" "all" {}

locals {
  inference_profiles = {
    for p in data.aws_bedrock_inference_profiles.system.inference_profile_summaries : p.inference_profile_id => p
  }
  foundation_model_arns = {
    for m in data.aws_bedrock_foundation_models.all.model_summaries : m.model_id => m.model_arn
  }

  used_profiles = [
    for id in var.anthropic_model_ids : local.inference_profiles[id]
    if contains(keys(local.inference_profiles), id)
  ]
  plain_model_ids = [
    for id in var.anthropic_model_ids : id
    if !contains(keys(local.inference_profiles), id) && contains(keys(local.foundation_model_arns), id)
  ]
  unknown_model_ids = [
    for id in var.anthropic_model_ids : id
    if !contains(keys(local.inference_profiles), id) && !contains(keys(local.foundation_model_arns), id)
  ]

  profile_arns     = [for p in local.used_profiles : p.inference_profile_arn]
  plain_model_arns = [for id in local.plain_model_ids : local.foundation_model_arns[id]]

  # The foundation models each profile routes to, in every region it routes
  # to. A global profile also needs the region-less and the source-region
  # foundation-model ARNs, added here rather than trusting models[] to list
  # them.
  routed_model_arns = distinct(flatten([
    for p in local.used_profiles : concat(
      [for m in p.models : m.model_arn],
      startswith(p.inference_profile_id, "global.") ? flatten([
        for m in p.models : [
          "arn:${local.partition}:bedrock:::foundation-model/${regex("foundation-model/(.+)$", m.model_arn)[0]}",
          "arn:${local.partition}:bedrock:${var.aws_region}::foundation-model/${regex("foundation-model/(.+)$", m.model_arn)[0]}",
        ]
      ]) : [],
    )
  ]))
}

data "aws_iam_policy_document" "task" {
  statement {
    sid       = "InvokeConfiguredModels"
    actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
    resources = concat(local.profile_arns, local.plain_model_arns)
  }

  dynamic "statement" {
    for_each = length(local.profile_arns) > 0 ? [1] : []

    content {
      sid       = "InvokeRoutedFoundationModels"
      actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
      resources = local.routed_model_arns

      condition {
        test     = "StringEquals"
        variable = "bedrock:InferenceProfileArn"
        values   = local.profile_arns
      }
    }
  }

  dynamic "statement" {
    for_each = length(local.profile_arns) > 0 ? [1] : []

    content {
      sid       = "GetConfiguredInferenceProfiles"
      actions   = ["bedrock:GetInferenceProfile"]
      resources = local.profile_arns
    }
  }

  statement {
    sid       = "ListInferenceProfiles"
    actions   = ["bedrock:ListInferenceProfiles"]
    resources = ["*"]
  }

  statement {
    sid       = "CodexMantle"
    actions   = ["bedrock-mantle:CreateInference"]
    resources = ["arn:${local.partition}:bedrock-mantle:${var.codex_bedrock_region}:${local.account}:project/*"]

    condition {
      test     = "StringEquals"
      variable = "bedrock-mantle:Model"
      values   = [var.codex_bedrock_model]
    }
  }
}

resource "aws_iam_role_policy" "task" {
  name   = "bedrock"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json

  lifecycle {
    precondition {
      condition     = length(local.unknown_model_ids) == 0
      error_message = "Model id(s) ${join(", ", local.unknown_model_ids)} are neither a system-defined Bedrock inference profile nor a foundation model in ${var.aws_region}."
    }
  }
}
