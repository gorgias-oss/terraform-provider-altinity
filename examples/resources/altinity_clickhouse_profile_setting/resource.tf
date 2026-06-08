# A custom settings profile + a setting attached to it + a user that uses it.
#
# The three-resource pattern is REQUIRED: an empty profile (no settings
# attached) is metadata-only in ACM and is never propagated to ClickHouse's
# user_directories — any user referencing it fails with Code 180
# THERE_IS_NO_PROFILE. Attach at least one setting before referencing the
# profile from a user.

# 1. The profile.
resource "altinity_clickhouse_profile" "etl" {
  cluster_id  = altinity_clickhouse_cluster.example.id
  name        = "etl_writer_profile"
  description = "Permissive profile for ETL writers — relaxed memory + longer timeouts"
}

# 2. The setting(s) attached to the profile. Multiple settings on one profile
#    are fine; each is its own ACM row.
resource "altinity_clickhouse_profile_setting" "etl_max_memory" {
  profile_id = altinity_clickhouse_profile.etl.profile_id
  name       = "max_memory_usage"
  value      = "21474836480" # 20 GiB
}

resource "altinity_clickhouse_profile_setting" "etl_max_execution_time" {
  profile_id = altinity_clickhouse_profile.etl.profile_id
  name       = "max_execution_time"
  value      = "600" # 10 minutes
}

# 3. A user that uses the profile. The depends_on covers EVERY profile_setting
#    on the profile — terraform's graph only sees the profile_id dependency,
#    which is metadata-only in ACM (Code 180 race otherwise).
resource "altinity_clickhouse_user" "etl_writer" {
  cluster_id        = altinity_clickhouse_cluster.example.id
  name              = "etl_writer"
  password          = "set-via-random_password" # see the user resource example
  profile_id        = altinity_clickhouse_profile.etl.profile_id
  networks          = "::/0"
  databases         = [] # all databases
  access_management = false

  depends_on = [
    altinity_clickhouse_profile_setting.etl_max_memory,
    altinity_clickhouse_profile_setting.etl_max_execution_time,
  ]
}
