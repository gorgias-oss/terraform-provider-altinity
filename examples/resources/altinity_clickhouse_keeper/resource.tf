# A CH Keeper coordination cluster. Reference it from a cluster via keeper_name.
#
# `ha` is intentionally absent — it is Computed-only (ACM auto-promotes based on
# the bound cluster's replica count). Adding `ha = true` will fail at plan time.
resource "altinity_clickhouse_keeper" "example" {
  environment   = data.altinity_environment.this.id
  name          = "shared-keeper"
  instance_type = "e2-standard-2" # zookeeper-scoped node type (see altinity_node_types)

  # `zones` is optional and platform-specific. Omit to let ACM pick — or list
  # the environment's az list (see the altinity_zones data source) to spread
  # replicas explicitly. The literal values below are GCP zones; on AWS/Azure
  # use the corresponding zone codes.
  # zones = data.altinity_zones.this.zones
}
