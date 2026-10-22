terraform {
  required_version = ">= 1.11.0"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "3.4.0"
    }
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = "1.27.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.9.1"
    }
  }
  backend "kubernetes" {}
}

variable "namespace" {
  type        = string
  description = "Namespace of the Database custom resource; identity only."
}

variable "name" {
  type        = string
  description = "Name of the Database custom resource; identity only."
}

# Technical authentication is supplied through KUBE_* environment variables.
provider "kubernetes" {}

# PGPASSWORD is injected by the runner from the restricted admin Secret.
provider "postgresql" {
  host            = "postgres.infra-backends.svc.cluster.local"
  port            = 5432
  username        = "postgres"
  database        = "postgres"
  sslmode         = "disable"
  connect_timeout = 15
}

data "kubernetes_resource" "database" {
  api_version = "platform.example.com/v1"
  kind        = "Database"
  metadata {
    namespace = var.namespace
    name      = var.name
  }
}

locals {
  cr       = data.kubernetes_resource.database.object
  spec     = local.cr.spec
  identity = substr(sha256(local.cr.metadata.uid), 0, 12)
  database = "${substr(var.name, 0, 40)}_${local.identity}"
  username = "${substr(local.spec.owner, 0, 40)}_${local.identity}"
  host     = "postgres.infra-backends.svc.cluster.local"
}

resource "random_password" "login" {
  length  = 32
  special = false
}

resource "postgresql_role" "login" {
  name             = local.username
  login            = true
  password         = random_password.login.result
  connection_limit = local.spec.connectionLimit
}

resource "postgresql_database" "database" {
  name             = local.database
  owner            = postgresql_role.login.name
  connection_limit = local.spec.connectionLimit
}

resource "postgresql_extension" "extensions" {
  for_each = toset(local.spec.extensions)
  name     = each.value
  database = postgresql_database.database.name
}

resource "kubernetes_manifest" "connection" {
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name      = "${var.name}-connection"
      namespace = var.namespace
      ownerReferences = [{
        apiVersion = "platform.example.com/v1"
        kind       = "Database"
        name       = var.name
        uid        = local.cr.metadata.uid
      }]
    }
    type = "Opaque"
    data = {
      host     = base64encode(local.host)
      port     = base64encode("5432")
      database = base64encode(postgresql_database.database.name)
      username = base64encode(postgresql_role.login.name)
      password = base64encode(random_password.login.result)
      uri      = base64encode("postgresql://${postgresql_role.login.name}:${random_password.login.result}@${local.host}:5432/${postgresql_database.database.name}?sslmode=disable")
    }
  }
  depends_on = [postgresql_extension.extensions]
}

output "connection_secret" {
  description = "Secret reference; credentials are never emitted as outputs."
  value = {
    namespace = var.namespace
    name      = kubernetes_manifest.connection.manifest.metadata.name
  }
}

output "database" {
  description = "Provisioned PostgreSQL database name."
  value       = postgresql_database.database.name
}
