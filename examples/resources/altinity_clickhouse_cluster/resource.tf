# Generate the admin password instead of hard-coding it.
resource "random_password" "admin" {
  length  = 24
  special = false
}

# Discover valid instance types, versions, and storage classes from ACM —
# DO NOT hard-code these. ACM retires older values and per-platform offerings
# differ; resolving them at plan time keeps the cluster launchable. See
# `examples/complete/` for the full pattern.
data "altinity_node_types" "ch" {
  environment = data.altinity_environment.this.id
  scope       = "clickhouse"
}

data "altinity_clickhouse_versions" "ch" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
  major       = 25
  stream      = "altinity-stable"
}

data "altinity_storage_classes" "ch" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

# A minimal single-shard / single-replica ClickHouse cluster.
#
# Note the ACM API's loose typing reflected in the schema:
#   - shards / replicas / *_port are numbers
#   - size / memory are strings (GB / MB as strings)
#   - secure / zone_awareness / mysql_protocol are booleans
#
# `node_count` is intentionally omitted — it is Computed (= shards × replicas)
# and not a rescale trigger. Setting it explicitly is rejected at plan time
# when it disagrees with shards × replicas.
resource "altinity_clickhouse_cluster" "example" {
  # `environment` is the ACM environment id; resolve it by name with the
  # altinity_environment data source.
  environment = data.altinity_environment.this.id
  name        = "analytics"

  role          = "prod" # REQUIRED — "prod" (Production) or "dev" (Development)
  shards        = 1
  replicas      = 1
  node_type     = data.altinity_node_types.ch.node_types[0].code
  size          = "100" # storage size in GB
  storage_class = data.altinity_storage_classes.ch.storage_classes[0].code
  version       = data.altinity_clickhouse_versions.ch.latest

  secure         = true
  admin_password = random_password.admin.result # write-only at the API; kept in state, see docs

  # Coordination — pick ONE (see altinity_clickhouse_keeper):
  #   keeper_name = altinity_clickhouse_keeper.shared.name
  #   zookeeper   = "launch"  # ACM auto-creates a ZK ensemble

  # `timeouts` is a nested attribute (note the `=`), not a block.
  timeouts = {
    create = "30m"
    update = "20m"
    delete = "20m"
  }
}
