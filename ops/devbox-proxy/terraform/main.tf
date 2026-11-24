# One wildcard DNS record for the devbox apps: *.<subzone> -> the edge server.
# This configuration manages that single record and nothing else in the zone:
# no import, no other resource. Values (zone, name, address) and the state
# live outside this repository; see ../README.md ("DNS: wildcard record").
#
#   export CLOUDFLARE_API_TOKEN=...   # DNS edit on the zone; never written to disk here
#   terraform -chdir=<this dir> init
#   terraform -chdir=<this dir> plan  -var-file=<private>/terraform.tfvars -state=<private>/terraform.tfstate
#   terraform -chdir=<this dir> apply -var-file=<private>/terraform.tfvars -state=<private>/terraform.tfstate

terraform {
  required_version = ">= 1.5"
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

provider "cloudflare" {}

variable "zone_name" {
  description = "Zone that holds the record (e.g. example.com)"
  type        = string
}

variable "record_name" {
  description = "Full name of the wildcard record (e.g. *.dev9.example.com)"
  type        = string
  validation {
    condition     = startswith(var.record_name, "*.") && endswith(var.record_name, ".${var.zone_name}")
    error_message = "record_name must be a wildcard (*.something) inside zone_name."
  }
}

variable "address" {
  description = "IPv4 address the wildcard points to (the edge server on the private network)"
  type        = string
  validation {
    condition     = can(cidrhost("${var.address}/32", 0))
    error_message = "address must be an IPv4 address."
  }
}

variable "ttl" {
  description = "TTL in seconds"
  type        = number
  default     = 300
}

data "cloudflare_zone" "this" {
  filter = {
    name = var.zone_name
  }
}

resource "cloudflare_dns_record" "devbox_wildcard" {
  zone_id = data.cloudflare_zone.this.zone_id
  name    = var.record_name
  type    = "A"
  content = var.address
  ttl     = var.ttl
  proxied = false
  comment = "devbox apps wildcard; managed by Terraform (OpenPoet ops/devbox-proxy/terraform)"
}

output "record" {
  value = "${cloudflare_dns_record.devbox_wildcard.name} A ${cloudflare_dns_record.devbox_wildcard.content}"
}
