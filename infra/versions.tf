terraform {
  # No backend block: state is local by default. See README.md for S3.
  required_version = "~> 1.12"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.71"
    }
  }
}
