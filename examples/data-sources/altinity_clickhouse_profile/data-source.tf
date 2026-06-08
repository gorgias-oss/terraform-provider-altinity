# Resolve a single profile by name on a cluster — fails at plan time if it
# doesn't exist. Use this when you want to reference a specific bootstrap
# profile (`default`/`readonly`) or an ACM-UI-created profile by name without
# having to filter the plural data source yourself.
data "altinity_clickhouse_profile" "readonly_bootstrap" {
  cluster_id = altinity_clickhouse_cluster.example.id
  name       = "readonly"
}

# A user that references the resolved profile by id. The depends_on chain
# guarantees the cluster is fully attached before lookup; ACM's `readonly`
# is auto-created at launch but the propagation isn't instant.
resource "altinity_clickhouse_user" "ro_user" {
  cluster_id        = altinity_clickhouse_cluster.example.id
  name              = "ro_user"
  password          = "set-via-random_password" # see the user resource example
  profile_id        = data.altinity_clickhouse_profile.readonly_bootstrap.profile_id
  networks          = "::/0"
  databases         = []
  access_management = false
}
