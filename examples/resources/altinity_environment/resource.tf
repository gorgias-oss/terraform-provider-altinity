# An Altinity.Cloud environment — the region-scoped unit that ClickHouse
# clusters are launched into. Created via the Altinity-hosted request flow and
# polled until ready (status "online").
#
# Resumable create: if provisioning exceeds the create timeout, the apply fails
# without recording state and a subsequent apply adopts the still-provisioning
# environment by name and resumes waiting — it is never destroyed and re-created.
#
# Guarded delete: destroying an environment that still contains clusters is
# refused. Destroy the clusters first.

# Discover the valid region codes for the chosen provider.
data "altinity_regions" "gcp" {
  cloud_provider = "gcp"
}

resource "altinity_environment" "example" {
  name           = "tf-demo-env"
  cloud_provider = "gcp"
  region         = "us-east1" # or e.g. data.altinity_regions.gcp.regions[0].code
  display_name   = "Terraform Demo"

  timeouts {
    create = "45m"
    delete = "30m"
  }
}
