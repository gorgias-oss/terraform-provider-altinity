# An Altinity.Cloud environment — the region-scoped unit that ClickHouse
# clusters are launched into. Created via the Altinity-hosted request flow and
# polled until ready (status "online").
#
# Resumable create: if provisioning exceeds the create timeout, the apply fails
# without recording state and a subsequent apply adopts the still-provisioning
# environment by name and resumes waiting — it is never destroyed and re-created.
#
# Destroy does NOT delete the environment in Altinity.Cloud — deletion requires
# an email + MFA confirmation that cannot be automated. `terraform destroy`
# removes the resource from state and warns; delete it manually in the ACM UI.

# Discover the valid region codes for the chosen provider.
data "altinity_regions" "gcp" {
  cloud_provider = "gcp"
}

variable "datadog_api_key" {
  type      = string
  sensitive = true
}

resource "altinity_environment" "example" {
  name           = "gorgias-tf-demo-env"
  cloud_provider = "gcp"
  region         = "us-east1" # or e.g. data.altinity_regions.gcp.regions[0].code
  display_name   = "Terraform Demo"

  # Datadog integration. `api_key` is write-only (sent, never read back, excluded
  # from drift detection). Omit the whole block to leave Datadog unmanaged.
  datadog = {
    enabled          = true
    api_key          = var.datadog_api_key
    region           = "datadoghq.com"
    send_metrics     = true
    send_logs        = true
    send_table_stats = false
    # apply_to_clusters defaults to true.
  }

  # Maintenance windows. ACM requires >= 48h over any 32-day window. Omit (null)
  # to leave unmanaged; set `[]` to clear all. Days are uppercase weekdays.
  maintenance_windows = [{
    name         = "weekend"
    enabled      = true
    hour         = 16
    length_hours = 8
    days         = ["FRIDAY", "SATURDAY", "SUNDAY"]
  }]

  # `timeouts` is a nested attribute (note the `=`), not a block.
  timeouts = {
    create = "45m"
    delete = "30m"
  }
}

output "altinity_env" {
  value     = altinity_environment.example
  sensitive = true # the resource now carries the write-only datadog api_key
}
