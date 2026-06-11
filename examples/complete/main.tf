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

provider "altinity" {
  api_token = var.altinity_api_token
  # api_url defaults to https://acm.altinity.cloud/api
}

# Resolve the target environment by name -> its ACM id.
data "altinity_environment" "this" {
  name = var.environment_name
}

# Generate random passwords so none are hard-coded. `result` is sensitive and
# is persisted to state (special = false keeps them shell/connection-string safe).
resource "random_password" "admin" {
  length  = 24
  special = false
}

resource "random_password" "analytics_ro" {
  length  = 24
  special = false
}

resource "random_password" "etl_writer" {
  length  = 24
  special = false
}

# Discover valid instance types. Keepers use the "zookeeper" scope; clusters use
# "clickhouse". This avoids hard-coding (and ACM's "Invalid Instance Type").
data "altinity_node_types" "keeper" {
  environment = data.altinity_environment.this.id
  scope       = "zookeeper"
}

data "altinity_node_types" "clickhouse" {
  environment = data.altinity_environment.this.id
  scope       = "clickhouse"
}

# Discover available ClickHouse versions (platform-specific!) and pick the
# latest, so `version` is never a hard-coded value ACM doesn't actually offer.
#
# `stream` selects the build line — one of:
#   altinity-stable  (Altinity Stable, *.altinitystable)
#   altinity-antalya (Project Antalya, *.altinityantalya)
#   upstream         (upstream ClickHouse)
# Filtering by a single stream keeps `latest` within that build line.
data "altinity_clickhouse_versions" "this" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
  major       = 25
  stream      = var.clickhouse_stream
}

# Discover available storage classes (e.g. pd-balanced, pd-ssd).
data "altinity_storage_classes" "this" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

# Discover availability zones for the cluster's azlist.
data "altinity_zones" "this" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
}

# A CH Keeper coordination cluster. A ClickHouse cluster must reference either a
# Keeper or ZooKeeper; we create one and attach the cluster to it by name.
resource "altinity_clickhouse_keeper" "demo" {
  environment   = data.altinity_environment.this.id
  name          = "tf-test-keeper"
  instance_type = data.altinity_node_types.keeper.node_types[0].code
}

# A minimal ClickHouse cluster (1 shard / 1 replica, smallest node type).
resource "altinity_clickhouse_cluster" "demo" {
  environment = data.altinity_environment.this.id
  name        = "tf-test"
  type        = data.altinity_environment.this.type # e.g. kubernetes

  role          = "prod" # "prod" (Production) or "dev" (Development) — REQUIRED by ACM
  shards        = 1
  replicas      = 1
  node_type     = data.altinity_node_types.clickhouse.node_types[0].code
  size          = "10"
  storage_class = data.altinity_storage_classes.this.storage_classes[0].code
  version       = data.altinity_clickhouse_versions.this.latest
  azlist        = data.altinity_zones.this.zones

  # host, ports, lb_type, secure, public_endpoint, mysql_*, disks, replicate_schema
  # all default to the ACM UI's values, so they don't need to be set here.

  # Coordination — pick ONE:
  #  - keeper_name: attach to the shared CH Keeper created above (used here).
  #  - zookeeper = "launch": let ACM auto-create a ZK ensemble (the ACM UI default).
  keeper_name = altinity_clickhouse_keeper.demo.name
  # zookeeper = "launch"

  secure         = true
  admin_user     = "admin" # required alongside admin_password (defaults to "admin")
  admin_password = random_password.admin.result

  timeouts = {
    create = "30m"
    update = "20m"
    delete = "20m"
  }
}


# A read-only settings profile on the cluster.
#
# IMPORTANT — two pitfalls:
#
#  1. Do NOT name a managed profile `default` or `readonly`. ACM auto-creates
#     and auto-maintains profiles with those reserved names at cluster launch
#     (and re-attaches its own settings to them). Trying to manage them from
#     Terraform produces opaque ACM errors — `{"data": false}` on edits,
#     sporadic 404s, etc. — because ACM's internal bookkeeping and Terraform
#     fight over the same row. Pick a project-specific name instead.
#
#  2. An EMPTY profile (no settings attached) is metadata-only in ACM and
#     never gets pushed to ClickHouse's user_directories — any user
#     referencing it then fails with Code 180 THERE_IS_NO_PROFILE. Attach at
#     least one setting via `altinity_clickhouse_profile_setting` so ACM
#     actually propagates the profile to the cluster.
resource "altinity_clickhouse_profile" "analytics_ro" {
  cluster_id  = altinity_clickhouse_cluster.demo.id
  name        = "analytics_ro_profile"
  description = "Read-only profile for the analytics_ro reporting user"
}

# Attach the canonical read-only setting. Without at least one setting, the
# profile is invisible to ClickHouse (see pitfall #2 above).
resource "altinity_clickhouse_profile_setting" "analytics_ro_readonly" {
  profile_id = altinity_clickhouse_profile.analytics_ro.profile_id
  name       = "readonly"
  value      = "1"
}

# A user that uses the read-only profile.
#
# The `depends_on` is BELT-AND-SUSPENDERS, not strictly required. The
# provider's user Create wraps the call in RetryOnTransientCreateRace, which
# retries on the propagation races (Code 62, 180, 511, 192, id=0, bare-bool
# data envelope) for up to ~9 minutes. So the user resource WILL eventually
# succeed even without `depends_on`. The explicit ordering trades a tiny bit
# of apply-graph parallelism for first-apply determinism: the user create
# fires only after the profile is fully attached, so the retry budget is
# almost never tapped in practice and the apply log stays clean.
#
# Skip `depends_on` if you don't mind seeing INFO-level "operator state
# still propagating; retrying" lines during the first apply; keep it for
# the quietest possible first-apply experience.
resource "altinity_clickhouse_user" "analytics_ro" {
  cluster_id = altinity_clickhouse_cluster.demo.id
  name       = "analytics_ro"
  password   = random_password.analytics_ro.result
  depends_on = [altinity_clickhouse_profile_setting.analytics_ro_readonly]

  # ACM canonicalizes networks server-side (e.g. "0.0.0.0/0" -> "::/0", the
  # ClickHouse match-all form), so use the canonical value here to keep the
  # configured value and ACM's view in sync. The configured value is
  # authoritative; out-of-band changes to networks/databases aren't drift-detected.
  networks = "::/0"
  # List of database NAMES the user can access (NOT `<db>.<table>` patterns —
  # ACM expands each entry to `GRANT ALL ON \`<db>\`.*` server-side). Omit
  # or pass `[]` to grant access to ALL databases.
  databases         = []
  access_management = false
  profile_id        = altinity_clickhouse_profile.analytics_ro.profile_id
}

# -----------------------------------------------------------------------------
# Demonstrate the altinity_clickhouse_profiles data source + a second profile
# with multiple custom settings + a user that uses it.
#
# Use case: an ETL writer with relaxed memory limits and longer query timeouts.
# This proves that profile_setting works with multiple entries and that the
# profiles data source can introspect what's on the cluster.
# -----------------------------------------------------------------------------

# A second profile, tuned for ETL writes. Multiple settings demonstrate that
# profile_setting works for more than just `readonly`.
resource "altinity_clickhouse_profile" "etl" {
  cluster_id  = altinity_clickhouse_cluster.demo.id
  name        = "etl_writer_profile"
  description = "Permissive profile for ETL writers — relaxed memory + longer timeouts"
}

resource "altinity_clickhouse_profile_setting" "etl_max_memory" {
  profile_id = altinity_clickhouse_profile.etl.profile_id
  name       = "max_memory_usage"
  value      = "21474836480" # 20 GiB
}

resource "altinity_clickhouse_profile_setting" "etl_max_execution_time" {
  profile_id = altinity_clickhouse_profile.etl.profile_id
  name       = "max_execution_time"
  value      = "600" # 10 minutes
}

# A writer user that uses the ETL profile.
resource "altinity_clickhouse_user" "etl_writer" {
  cluster_id = altinity_clickhouse_cluster.demo.id
  name       = "etl_writer"
  password   = random_password.etl_writer.result

  # Wait for ALL of the profile's settings to be attached before creating the
  # user. Without this, terraform's graph would only see the profile_id
  # dependency, which is metadata-only in ACM — Code 180 race.
  depends_on = [
    altinity_clickhouse_profile_setting.etl_max_memory,
    altinity_clickhouse_profile_setting.etl_max_execution_time,
  ]

  networks          = "::/0"
  databases         = [] # all databases (no GRANT restriction)
  access_management = false
  profile_id        = altinity_clickhouse_profile.etl.profile_id
}

# Look up everything ACM has on the cluster — bootstrap profiles
# (`default`/`readonly`) plus the ones we manage above.
#
# The depends_on MUST list every profile/profile_setting we manage. The data
# source reads at the end of the apply graph, but only AFTER each declared
# dependency. Without an exhaustive list, terraform may read this data source
# in parallel with the still-creating profiles — they'd be missing from the
# returned list on the first apply, and the next refresh would show a
# spurious output diff.
data "altinity_clickhouse_profiles" "all" {
  cluster_id = altinity_clickhouse_cluster.demo.id
  depends_on = [
    altinity_clickhouse_profile_setting.analytics_ro_readonly,
    altinity_clickhouse_profile_setting.etl_max_memory,
    altinity_clickhouse_profile_setting.etl_max_execution_time,
  ]
}

# Sanity-extract the ACM-bootstrap profile ids without hard-coding them.
locals {
  bootstrap_default_profile_id  = one([for p in data.altinity_clickhouse_profiles.all.profiles : p.profile_id if p.name == "default"])
  bootstrap_readonly_profile_id = one([for p in data.altinity_clickhouse_profiles.all.profiles : p.profile_id if p.name == "readonly"])
  tf_managed_profile_names      = [for p in data.altinity_clickhouse_profiles.all.profiles : p.name if !contains(["default", "readonly"], p.name)]
}

# Singular lookup: resolve ONE profile by name. Use this when you want to
# reference a specific bootstrap or ACM-UI-created profile without filtering
# the full list yourself. The data source errors at plan time if the profile
# doesn't exist — safer than the `one([...])` filter on the plural data source,
# which silently returns null and propagates downstream as confusing errors.
data "altinity_clickhouse_profile" "etl_lookup" {
  cluster_id = altinity_clickhouse_cluster.demo.id
  name       = "etl_writer_profile"
  depends_on = [
    altinity_clickhouse_profile_setting.etl_max_memory,
    altinity_clickhouse_profile_setting.etl_max_execution_time,
  ]
}

# A cluster-level setting.
resource "altinity_clickhouse_cluster_setting" "max_queries" {
  cluster_id = altinity_clickhouse_cluster.demo.id
  name       = "max_concurrent_queries"
  value      = "200"
}
