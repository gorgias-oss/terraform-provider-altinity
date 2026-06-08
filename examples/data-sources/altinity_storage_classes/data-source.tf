# List available storage classes for an environment (e.g. pd-balanced, pd-ssd).
data "altinity_storage_classes" "this" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

output "storage_class_codes" {
  value = [for sc in data.altinity_storage_classes.this.storage_classes : sc.code]
}
