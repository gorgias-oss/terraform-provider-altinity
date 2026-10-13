# A single cluster-level setting.
resource "altinity_clickhouse_cluster_setting" "example" {
  cluster_id = altinity_clickhouse_cluster.example.id
  name       = "max_concurrent_queries"
  value      = "200"
}

# A setting whose value comes from a Kubernetes secret in the cluster's
# namespace, in the form "<secret-name>/<key>".
resource "altinity_clickhouse_cluster_setting" "interserver_password" {
  cluster_id = altinity_clickhouse_cluster.example.id
  name       = "interserver_http_credentials/password"
  value_from = "clickhouse-interserver/password"
}
