# Resolve an existing Altinity.Cloud environment by name. The computed `id` is
# what the altinity_clickhouse_cluster resource's `environment` argument expects.
data "altinity_environment" "this" {
  name = "your-environment-name"
}

output "environment_id" {
  value = data.altinity_environment.this.id
}
