# List the instance types available in an environment, filtered by scope.
# Use scope "clickhouse" for cluster node_type, "zookeeper" for keeper
# instance_type.
data "altinity_node_types" "keeper" {
  environment = data.altinity_environment.this.id
  scope       = "zookeeper"
}

output "keeper_instance_type_codes" {
  value = [for nt in data.altinity_node_types.keeper.node_types : nt.code]
}
