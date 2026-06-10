# List the instance types available for a cloud provider in a region. Use an
# instance type's name as an altinity_node_type `code`.
data "altinity_instance_types" "gcp" {
  cloud_provider = "gcp"
  region         = "us-east1"
}

output "instance_type_names" {
  value = [for t in data.altinity_instance_types.gcp.instance_types : t.name]
}

output "zones" {
  value = data.altinity_instance_types.gcp.zones
}
