# A single cluster-level setting.
resource "altinity_clickhouse_cluster_setting" "example" {
  cluster_id = altinity_clickhouse_cluster.example.id
  name       = "max_concurrent_queries"
  value      = "200"
}
