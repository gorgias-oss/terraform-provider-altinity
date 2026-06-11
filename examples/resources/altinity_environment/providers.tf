terraform {
  required_version = ">= 1.5.7"
  required_providers {
    altinity = {
      source  = "gorgias-oss/altinity"
      version = ">= 0.1.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.0"
    }
  }
}
