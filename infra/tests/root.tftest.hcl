# Plans the root module against a mocked AWS provider: no credentials, no AWS
# calls. Checks the variables-to-container-env mapping and input validation.

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
      names = ["eu-central-1a", "eu-central-1b"]
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  # An empty profile list keeps the mock from generating random entries.
  mock_data "aws_bedrock_inference_profiles" {
    defaults = {
      inference_profile_summaries = []
    }
  }

  mock_data "aws_bedrock_foundation_models" {
    defaults = {
      model_summaries = [
        {
          customizations_supported     = []
          inference_types_supported    = ["ON_DEMAND"]
          input_modalities             = ["TEXT"]
          model_arn                    = "arn:aws:bedrock:eu-central-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0"
          model_id                     = "anthropic.claude-3-haiku-20240307-v1:0"
          model_name                   = "Claude 3 Haiku"
          output_modalities            = ["TEXT"]
          provider_name                = "Anthropic"
          response_streaming_supported = true
        },
      ]
    }
  }

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

  mock_resource "aws_ecs_cluster" {
    defaults = {
      arn = "arn:aws:ecs:eu-central-1:123456789012:cluster/heinzel"
    }
  }

  mock_resource "aws_acm_certificate" {
    defaults = {
      arn = "arn:aws:acm:eu-central-1:123456789012:certificate/mock"
    }
  }

  mock_resource "aws_lb" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/heinzel/mock"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:targetgroup/heinzel/mock"
    }
  }

  mock_resource "aws_lb_listener" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:listener/app/heinzel/mock/mock"
    }
  }
}

variables {
  aws_region          = "eu-central-1"
  domain_name         = "heinzel.example.com"
  hosted_zone_id      = "Z0123456789ABCDEFGHIJ"
  image_tag           = "0123456789ab"
  sentry_github_repos = "1=acme/app"
  anthropic_model     = "anthropic.claude-3-haiku-20240307-v1:0"
  codex_bedrock_model = "openai.gpt-oss-120b"
}

run "null_optionals_are_left_out_of_the_env" {
  command = plan

  assert {
    condition = tomap(local.container_environment) == tomap({
      AWS_REGION          = "eu-central-1"
      SENTRY_GITHUB_REPOS = "1=acme/app"
      ANTHROPIC_MODEL     = "anthropic.claude-3-haiku-20240307-v1:0"
      CODEX_BEDROCK_MODEL = "openai.gpt-oss-120b"
    })
    error_message = "env: ${jsonencode(local.container_environment)}"
  }

  assert {
    condition     = output.webhook_url == "https://heinzel.example.com/sentry/callback"
    error_message = "webhook_url: ${output.webhook_url}"
  }
}

run "set_optionals_reach_the_env" {
  command = plan

  variables {
    investigation_timeout      = "20m"
    anthropic_small_fast_model = "anthropic.claude-3-haiku-20240307-v1:0"
    codex_bedrock_region       = "us-west-2"
  }

  assert {
    condition = tomap(local.container_environment) == tomap({
      AWS_REGION                 = "eu-central-1"
      SENTRY_GITHUB_REPOS        = "1=acme/app"
      INVESTIGATION_TIMEOUT      = "20m"
      ANTHROPIC_MODEL            = "anthropic.claude-3-haiku-20240307-v1:0"
      ANTHROPIC_SMALL_FAST_MODEL = "anthropic.claude-3-haiku-20240307-v1:0"
      CODEX_BEDROCK_MODEL        = "openai.gpt-oss-120b"
      CODEX_BEDROCK_REGION       = "us-west-2"
    })
    error_message = "env: ${jsonencode(local.container_environment)}"
  }
}

run "invalid_inputs_are_rejected" {
  command = plan

  variables {
    aws_region            = "EU-Central-1"
    anthropic_model       = "bad model"
    codex_bedrock_region  = "nowhere"
    investigation_timeout = "30 minutes"
  }

  expect_failures = [
    var.aws_region,
    var.anthropic_model,
    var.codex_bedrock_region,
    var.investigation_timeout,
  ]
}
