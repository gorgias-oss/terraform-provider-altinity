# A settings profile that users can be attached to.
#
# IMPORTANT: do NOT use `default` or `readonly` as the profile name. ACM
# auto-creates and auto-maintains profiles with those reserved names at
# cluster launch; trying to manage them from Terraform produces opaque
# errors (`{"data": false}`, sporadic 404s) because ACM's bookkeeping and
# Terraform fight over the same row. The provider rejects them at plan time.
# Pick a project-specific name instead.
#
# An EMPTY profile (no settings attached) is metadata-only in ACM and is
# never propagated to ClickHouse's user_directories. Attach at least one
# `altinity_clickhouse_profile_setting` (see the
# altinity_clickhouse_profile_setting example) before referencing this
# profile from a user, or user Create will fail with Code 180
# THERE_IS_NO_PROFILE.
resource "altinity_clickhouse_profile" "example" {
  cluster_id  = altinity_clickhouse_cluster.example.id
  name        = "analytics_ro_profile"
  description = "Read-only settings profile for the analytics_ro reporting user"
}
