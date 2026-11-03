# Plans module.base against a mocked AWS provider: no credentials, no AWS
# calls. Checks how model ids become task-role grants.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
    }
  }

  # Mocked ARNs are random strings unless set; resources that take an ARN
  # validate it at plan time.
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock"
    }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:mock"
    }
  }

  # A mocked policy document's json would be random; any valid policy will
  # do, since the tests check the document's statements, not its json.
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  mock_data "aws_bedrock_inference_profiles" {
    defaults = {
      inference_profile_summaries = [
        {
          created_at             = "2025-09-29T00:00:00Z"
          description            = ""
          inference_profile_name = "Global Claude Sonnet 4.5"
          status                 = "ACTIVE"
          type                   = "SYSTEM_DEFINED"
          updated_at             = "2025-09-29T00:00:00Z"
          inference_profile_id   = "global.anthropic.claude-sonnet-4-5-20250929-v1:0"
          inference_profile_arn  = "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/global.anthropic.claude-sonnet-4-5-20250929-v1:0"
          # Only the region-less ARN, so the test proves the code adds the
          # source-region one itself.
          models = [
            { model_arn = "arn:aws:bedrock:::foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0" },
          ]
        },
        {
          created_at             = "2025-09-29T00:00:00Z"
          description            = ""
          inference_profile_name = "EU Claude Haiku 4.5"
          status                 = "ACTIVE"
          type                   = "SYSTEM_DEFINED"
          updated_at             = "2025-09-29T00:00:00Z"
          inference_profile_id   = "eu.anthropic.claude-haiku-4-5-20251001-v1:0"
          inference_profile_arn  = "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-haiku-4-5-20251001-v1:0"
          models = [
            { model_arn = "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0" },
            { model_arn = "arn:aws:bedrock:eu-west-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0" },
          ]
        },
      ]
    }
  }

  mock_data "aws_bedrock_foundation_models" {
    defaults = {
      model_summaries = [
        {
          customizations_supported     = []
          inference_types_supported    = ["ON_DEMAND"]
          input_modalities             = ["TEXT"]
          model_name                   = "Claude 3 Haiku"
          output_modalities            = ["TEXT"]
          provider_name                = "Anthropic"
          response_streaming_supported = true
          model_id                     = "anthropic.claude-3-haiku-20240307-v1:0"
          model_arn                    = "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0"
        },
      ]
    }
  }
}

variables {
  name                      = "heinzel"
  aws_region                = "eu-central-1"
  log_group_name            = "/ecs/heinzel"
  github_token_secret_name  = "heinzel/github-token"
  sentry_client_secret_name = "heinzel/sentry-client-secret"
  image_tag                 = "abc123"
  codex_bedrock_model       = "openai.gpt-oss-120b"
  codex_bedrock_region      = "us-west-2"
}

run "global_profile_and_foundation_model" {
  command = plan

  module {
    source = "./modules/base"
  }

  variables {
    anthropic_model_ids = [
      "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
      "anthropic.claude-3-haiku-20240307-v1:0",
    ]
  }

  assert {
    condition = tolist(local.profile_arns) == tolist([
      "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/global.anthropic.claude-sonnet-4-5-20250929-v1:0",
    ])
    error_message = "profile ARNs: ${jsonencode(local.profile_arns)}"
  }

  assert {
    condition = tolist(local.plain_model_arns) == tolist([
      "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0",
    ])
    error_message = "plain model ARNs: ${jsonencode(local.plain_model_arns)}"
  }

  assert {
    condition = tolist(local.routed_model_arns) == tolist([
      "arn:aws:bedrock:::foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0",
      "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0",
    ])
    error_message = "routed model ARNs: ${jsonencode(local.routed_model_arns)}"
  }

  assert {
    condition = tolist([for s in data.aws_iam_policy_document.task.statement : s.sid]) == tolist([
      "InvokeConfiguredModels",
      "InvokeRoutedFoundationModels",
      "GetConfiguredInferenceProfiles",
      "ListInferenceProfiles",
      "CodexMantle",
    ])
    error_message = "statements: ${jsonencode([for s in data.aws_iam_policy_document.task.statement : s.sid])}"
  }

  assert {
    condition     = tolist(one(data.aws_iam_policy_document.task.statement[1].condition).values) == sort(tolist(local.profile_arns))
    error_message = "routed foundation models must be conditioned on the configured profile ARNs"
  }

  assert {
    condition = tolist(data.aws_iam_policy_document.task.statement[4].resources) == tolist([
      "arn:aws:bedrock-mantle:us-west-2:123456789012:project/*",
    ])
    error_message = "Mantle resource must use the Codex region"
  }

  assert {
    condition     = tolist(one(data.aws_iam_policy_document.task.statement[4].condition).values) == tolist(["openai.gpt-oss-120b"])
    error_message = "Mantle grant must be conditioned on the Codex model"
  }
}

run "geo_profile_routes_to_every_region" {
  command = plan

  module {
    source = "./modules/base"
  }

  variables {
    anthropic_model_ids = ["eu.anthropic.claude-haiku-4-5-20251001-v1:0"]
  }

  assert {
    condition = tolist(local.routed_model_arns) == tolist([
      "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0",
      "arn:aws:bedrock:eu-west-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0",
    ])
    error_message = "routed model ARNs: ${jsonencode(local.routed_model_arns)}"
  }
}

run "foundation_model_only_has_no_profile_statements" {
  command = plan

  module {
    source = "./modules/base"
  }

  variables {
    anthropic_model_ids = ["anthropic.claude-3-haiku-20240307-v1:0"]
  }

  assert {
    condition = tolist([for s in data.aws_iam_policy_document.task.statement : s.sid]) == tolist([
      "InvokeConfiguredModels",
      "ListInferenceProfiles",
      "CodexMantle",
    ])
    error_message = "statements: ${jsonencode([for s in data.aws_iam_policy_document.task.statement : s.sid])}"
  }
}

run "unknown_model_fails_the_plan" {
  command = plan

  module {
    source = "./modules/base"
  }

  variables {
    anthropic_model_ids = ["us.anthropic.claude-nope-v1:0"]
  }

  expect_failures = [aws_iam_role_policy.task]
}
