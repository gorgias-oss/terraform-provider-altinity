# List ALL profiles on a cluster — ACM bootstrap profiles (`default`,
# `readonly`) plus any custom profiles created via the UI or this provider.
#
# The depends_on MUST list every profile/profile_setting we manage in this
# module. The data source reads at the end of the apply graph, but only after
# its declared dependencies — without exhaustive deps it may read in parallel
# with still-creating profiles and miss them on the first apply, then show
# spurious diffs on the next refresh.
data "altinity_clickhouse_profiles" "all" {
  cluster_id = altinity_clickhouse_cluster.example.id
  depends_on = [
    altinity_clickhouse_profile.analytics_ro,
    altinity_clickhouse_profile_setting.analytics_ro_readonly,
  ]
}

# Sanity-extract bootstrap profile ids without hard-coding them.
output "default_profile_id" {
  value = one([for p in data.altinity_clickhouse_profiles.all.profiles : p.profile_id if p.name == "default"])
}

# List only the Terraform-managed (non-bootstrap) profiles.
output "managed_profile_names" {
  value = [for p in data.altinity_clickhouse_profiles.all.profiles : p.name if !contains(["default", "readonly"], p.name)]
}
