# An environment node type (instance shape) a cluster can be scheduled onto.
#
# Tolerations / nodeSelector / extraSpec are NOT managed here: on create the
# provider mirrors the ACM UI's per-scope default tolerations, and on update it
# preserves whatever ACM has. `used` is read-only (true when a cluster uses it).

# Discover the available instance types for the environment's provider+region.
data "altinity_instance_types" "gcp" {
  cloud_provider = "gcp"
  region         = "us-east1"
}

locals {
  # Pick a specific instance type from the catalog.
  chosen = one([for t in data.altinity_instance_types.gcp.instance_types : t if t.name == "n2d-standard-16"])
}

resource "altinity_node_type" "clickhouse_16" {
  environment = altinity_environment.example.id
  scope       = "clickhouse"
  code        = local.chosen.name
  cpu         = local.chosen.cpu
  memory      = local.chosen.memory * 1024 # data source is GiB; node type wants MB
  capacity    = 10
  # name     = "analytics-pool" # optional; applied via a follow-up edit
}
