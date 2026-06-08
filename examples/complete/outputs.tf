output "cluster_id" {
  value = altinity_clickhouse_cluster.demo.id
}

output "cluster_endpoint" {
  value = altinity_clickhouse_cluster.demo.endpoint
}

output "cluster_endpoint_http" {
  value = altinity_clickhouse_cluster.demo.endpoint_http
}

# The selected ClickHouse version and its build stream, so it's clear which
# build (Altinity Stable vs Project Antalya vs upstream) the cluster runs.
output "clickhouse_version" {
  value = altinity_clickhouse_cluster.demo.version
}

output "clickhouse_build_stream" {
  description = "altinity-stable | altinity-antalya | clickhouse-lts | clickhouse"
  value       = try([for v in data.altinity_clickhouse_versions.this.versions : v.stream if v.code == altinity_clickhouse_cluster.demo.version][0], "unknown")
}

# Generated passwords. Retrieve with: terraform output -raw admin_password
output "admin_password" {
  value     = random_password.admin.result
  sensitive = true
}

output "analytics_ro_password" {
  value     = random_password.analytics_ro.result
  sensitive = true
}

output "etl_writer_password" {
  value     = random_password.etl_writer.result
  sensitive = true
}

# Demonstrates the altinity_clickhouse_profiles data source — full profile list
# on the cluster (both ACM-bootstrapped and terraform-managed). After apply you
# should see at least: default, readonly, analytics_ro_profile, etl_writer_profile.
output "cluster_profiles" {
  description = "All settings profiles present on the cluster"
  value = [
    for p in data.altinity_clickhouse_profiles.all.profiles :
    "${p.name} (id=${p.profile_id})"
  ]
}

# Bootstrap profile ids discovered via the data source rather than hard-coded.
output "bootstrap_profile_ids" {
  description = "ACM-managed bootstrap profile ids, looked up via the data source"
  value = {
    default  = local.bootstrap_default_profile_id
    readonly = local.bootstrap_readonly_profile_id
  }
}

# The profiles we own (filter out ACM's bootstrap names).
output "tf_managed_profile_names" {
  description = "Profile names created/adopted by Terraform on this cluster"
  value       = local.tf_managed_profile_names
}

# Demonstrates the singular altinity_clickhouse_profile data source —
# one profile resolved by name, with its description from ACM.
output "etl_profile_lookup" {
  description = "Result of looking up etl_writer_profile via the singular data source"
  value = {
    profile_id  = data.altinity_clickhouse_profile.etl_lookup.profile_id
    name        = data.altinity_clickhouse_profile.etl_lookup.name
    description = data.altinity_clickhouse_profile.etl_lookup.description
  }
}
