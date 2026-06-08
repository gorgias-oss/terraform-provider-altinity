# List availability zones in an environment (for cluster azlist / keeper zones).
data "altinity_zones" "this" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

output "zones" {
  value = data.altinity_zones.this.zones
}
