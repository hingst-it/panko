# Test-only module for the cloud proof (roadmap 8c): one S3 bucket against an emulated AWS API.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Later 6.x releases send bucket tags with CreateBucket, which Moto ignores; every plan would
    # then report drift.
    aws = {
      source  = "hashicorp/aws"
      version = "6.20.0"
    }
  }
}

variable "namespace" {
  type        = string
  description = "Namespace of the custom resource; identity only."
}

variable "name" {
  type        = string
  description = "Name of the custom resource; identity only."
}

variable "resource" {
  type        = any
  description = "Immutable resource snapshot captured by the controller, without credentials or status."
  validation {
    condition     = var.resource.metadata.namespace == var.namespace && var.resource.metadata.name == var.name
    error_message = "Snapshot identity must match the requested resource."
  }
}

# Endpoint and dummy credentials come from the agent's local profile (AWS_ENDPOINT_URL for all
# services, so no request reaches real AWS; AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY). The emulator
# has no account, STS or metadata service.
provider "aws" {
  region                      = "us-east-1"
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
}

locals {
  cr = var.resource
  # A restored resource keeps its original identity, so the bucket name stays stable.
  identity = substr(sha256(try(local.cr.metadata.identity, local.cr.metadata.uid)), 0, 12)
  bucket   = "panko-${substr(var.name, 0, 40)}-${local.identity}"
  tags     = merge(try(local.cr.spec.tags, {}), { "managed-by" = "panko" })
}

resource "aws_s3_bucket" "this" {
  bucket = local.bucket
  tags   = local.tags
}

output "bucket" {
  value = aws_s3_bucket.this.bucket
}
