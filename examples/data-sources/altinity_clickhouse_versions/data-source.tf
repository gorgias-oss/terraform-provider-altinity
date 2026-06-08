# List available ClickHouse versions for an environment and pick one.
# `platform` is important — without it ACM returns a stale/older list.
data "altinity_clickhouse_versions" "all" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

# Filter to the latest Altinity Stable build in the 25.x line.
# stream is one of: altinity-stable | altinity-antalya | upstream. Filtering by
# a single stream keeps `latest` within that build line.
data "altinity_clickhouse_versions" "altinity_25" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
  major       = 25
  stream      = "altinity-stable"
}

output "latest_version" {
  value = data.altinity_clickhouse_versions.all.latest
}

output "latest_altinity_25" {
  value = data.altinity_clickhouse_versions.altinity_25.latest
}

# Full detail per version, including build stream.
output "version_streams" {
  value = [for v in data.altinity_clickhouse_versions.all.versions :
    { code = v.code, stream = v.stream, is_eol = v.is_eol }
  ]
}
