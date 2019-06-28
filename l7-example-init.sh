#!/bin/bash

set -euo pipefail

unset CDPATH

cd "$(dirname "$0")"

export CONSUL_HTTP_TOKEN=root

#####################################################
#
# service:web
#
consul config write - << EOF
Kind = "service-router"
Name = "web"
Routes = [
  {
    Match {
      HTTP {
        PathPrefix    = "/admin"
        PrefixRewrite = "/"
      }
    }

    Destination {
      Service = "admin"
    }
  },
]
EOF
consul config write - << EOF
Kind = "service-splitter"
Name = "web"
Splits = [
  {
    Weight  = 50
    Service = "web"
  },
  {
    Weight  = 50
    Service = "web-haskell"
  },
]
EOF
consul config write - << EOF
Kind = "service-resolver"
Name = "web"
Failover {
  "*" = {
    Datacenters = ["dc2"]
  }
}
EOF

#####################################################
#
# service:admin
#
consul config delete -kind service-router -name admin
consul config write - << EOF
Kind = "service-splitter"
Name = "admin"
Splits = [
  {
    Weight        = 50
    ServiceSubset = "v2"
  },
  {
    Weight        = 50
    ServiceSubset = "v1"
  },
]
EOF
consul config write - << EOF
Kind = "service-resolver"
Name = "admin"
Subsets = {
  "v2" = {
    Filter      = "ServiceMeta.version == 2"
    OnlyPassing = true
  }

  "v1" = {
    Filter = "ServiceMeta.version == 1"
  }
}
EOF

#####################################################
#
# service:web-haskell
#
consul config delete -kind service-router -name web-haskell
consul config delete -kind service-splitter -name web-haskell
consul config delete -kind service-resolver -name web-haskell

#####################################################
## CONFIG DUMP
#####################################################

echo "---"
for kind in service-router service-splitter service-resolver; do
    for name in $(consul config list -kind "$kind"); do
        echo "---"
        consul config read -kind "$kind" -name "$name"
    done
done
