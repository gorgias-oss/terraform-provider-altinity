# Generate the user password instead of hard-coding it.
resource "random_password" "analytics_ro" {
  length  = 24
  special = false
}

# A read-only database user attached to a cluster.
resource "altinity_clickhouse_user" "example" {
  cluster_id = altinity_clickhouse_cluster.example.id
  name       = "analytics_ro"
  password   = random_password.analytics_ro.result # write-only at the API

  networks = "0.0.0.0/0"
  # List of database NAMES (not `<db>.<table>` patterns). ACM expands each
  # entry to `GRANT ALL ON \`<db>\`.*` server-side. Omit / pass `[]` to grant
  # access to ALL databases (`*.*`).
  databases         = ["default", "analytics"]
  access_management = false

  # Optional: attach a settings profile by id. The profile MUST have at least
  # one altinity_clickhouse_profile_setting attached — an empty profile is
  # metadata-only in ACM and a referencing user fails with Code 180
  # THERE_IS_NO_PROFILE on Create.
  # profile_id = altinity_clickhouse_profile.analytics_ro.profile_id
}
