# Node types are imported by "<environment>:<scope>:<code>" (scope is
# clickhouse, zookeeper, or system) — NOT by the state id
# "<environment>:<node_type_id>". The numeric node-type id is resolved on
# the first refresh after import.
terraform import altinity_node_type.example 2267:clickhouse:n2d-standard-16
