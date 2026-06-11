# List the regions available for a cloud provider. Use a region code as an
# altinity_environment's `region`.
data "altinity_regions" "aws" {
  cloud_provider = "aws"
}

# Example: reference the first region's code.
# region = data.altinity_regions.aws.regions[0].code

output "aws_region_codes" {
  value = [for r in data.altinity_regions.aws.regions : r.code]
}
