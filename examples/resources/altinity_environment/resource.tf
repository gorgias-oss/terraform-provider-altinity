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

resource "altinity_environment" "example" {
  name           = "gorgias-tf-demo-env"
  cloud_provider = "gcp"
  region         = "us-east1" # or e.g. data.altinity_regions.gcp.regions[0].code
  display_name   = "Terraform Demo"

  # `timeouts` is a nested attribute (note the `=`), not a block.
  timeouts = {
    create = "45m"
    delete = "30m"
  }
}

output "altinity_regions" {
  value = data.altinity_regions.gcp.regions
}
