package xds

import (
	"errors"
	"fmt"

	envoy "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoycore "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoyendpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/proto"

	"github.com/hashicorp/consul/agent/proxycfg"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
)

// endpointsFromSnapshot returns the xDS API representation of the "endpoints"
func endpointsFromSnapshot(cfgSnap *proxycfg.ConfigSnapshot, token string) ([]proto.Message, error) {
	if cfgSnap == nil {
		return nil, errors.New("nil config given")
	}

	switch cfgSnap.Kind {
	case structs.ServiceKindConnectProxy:
		return endpointsFromSnapshotConnectProxy(cfgSnap, token)
	default:
		return nil, fmt.Errorf("Invalid service kind: %v", cfgSnap.Kind)
	}
}

// endpointsFromSnapshotConnectProxy returns the xDS API representation of the "endpoints"
// (upstream instances) in the snapshot.
func endpointsFromSnapshotConnectProxy(cfgSnap *proxycfg.ConfigSnapshot, token string) ([]proto.Message, error) {
	if cfgSnap == nil {
		return nil, errors.New("nil config given")
	}
	// TODO(rb): this sizing is a low bound.

	resources := make([]proto.Message, 0, len(cfgSnap.UpstreamEndpoints))

	// TODO(rb): should naming from 1.5 -> 1.6 for clusters remain unchanged?

	for _, u := range cfgSnap.Proxy.Upstreams {
		id := u.Identifier()

		var chain *structs.CompiledDiscoveryChain
		if u.DestinationType != structs.UpstreamDestTypePreparedQuery {
			chain = cfgSnap.DiscoveryChain[id]
		}

		if chain == nil || chain.IsDefault() {
			// Either old-school upstream or prepared query.

			endpoints, ok := cfgSnap.UpstreamEndpoints[id]
			if ok {
				la := makeLoadAssignment(id, endpoints)
				resources = append(resources, la)
			}

		} else {
			// Newfangled discovery chain plumbing.

			chainEndpointMap, ok := cfgSnap.WatchedUpstreamEndpoints[id]
			if !ok {
				continue // TODO(rb): whaaaa?
			}

			for target, node := range chain.GroupResolverNodes {
				groupResolver := node.GroupResolver
				failover := groupResolver.Failover

				endpoints, ok := chainEndpointMap[target]
				if !ok {
					continue // TODO(rb): whaaaa?
				}

				var (
					priorityEndpoints      []structs.CheckServiceNodes
					overprovisioningFactor int
				)

				if failover != nil && len(failover.Targets) > 0 {
					priorityEndpoints = make([]structs.CheckServiceNodes, 0, len(failover.Targets)+1)

					priorityEndpoints = append(priorityEndpoints, endpoints)

					if failover.Definition.OverprovisioningFactor > 0 {
						overprovisioningFactor = failover.Definition.OverprovisioningFactor
					}
					if overprovisioningFactor <= 0 {
						// We choose such a large value here that the failover math should
						// in effect not happen until zero instances are healthy.
						overprovisioningFactor = 100000
					}

					for _, failTarget := range failover.Targets {
						failEndpoints, ok := chainEndpointMap[failTarget]
						if ok {
							priorityEndpoints = append(priorityEndpoints, failEndpoints)
						}
					}
				} else {
					priorityEndpoints = []structs.CheckServiceNodes{
						endpoints,
					}
				}

				clusterName := makeClusterName(id, target, cfgSnap.Datacenter)

				la := makeLoadAssignmentForDiscoveryChain(
					clusterName,
					chain,
					overprovisioningFactor,
					priorityEndpoints,
				)
				resources = append(resources, la)
			}
		}
	}

	return resources, nil
}

func makeEndpoint(clusterName, host string, port int) envoyendpoint.LbEndpoint {
	return envoyendpoint.LbEndpoint{
		HostIdentifier: &envoyendpoint.LbEndpoint_Endpoint{
			Endpoint: &envoyendpoint.Endpoint{
				Address: makeAddressPtr(host, port),
			},
		},
	}
}

func makeLoadAssignment(clusterName string, endpoints structs.CheckServiceNodes) *envoy.ClusterLoadAssignment {
	es := make([]envoyendpoint.LbEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		addr := ep.Service.Address
		if addr == "" {
			addr = ep.Node.Address
		}
		healthStatus := envoycore.HealthStatus_HEALTHY
		weight := 1
		if ep.Service.Weights != nil {
			weight = ep.Service.Weights.Passing
		}

		for _, chk := range ep.Checks {
			if chk.Status == api.HealthCritical {
				// This can't actually happen now because health always filters critical
				// but in the future it may not so set this correctly!
				healthStatus = envoycore.HealthStatus_UNHEALTHY
			}
			if chk.Status == api.HealthWarning && ep.Service.Weights != nil {
				weight = ep.Service.Weights.Warning
			}
		}
		// Make weights fit Envoy's limits. A zero weight means that either Warning
		// (likely) or Passing (weirdly) weight has been set to 0 effectively making
		// this instance unhealthy and should not be sent traffic.
		if weight < 1 {
			healthStatus = envoycore.HealthStatus_UNHEALTHY
			weight = 1
		}
		if weight > 128 {
			weight = 128
		}
		es = append(es, envoyendpoint.LbEndpoint{
			HostIdentifier: &envoyendpoint.LbEndpoint_Endpoint{
				Endpoint: &envoyendpoint.Endpoint{
					Address: makeAddressPtr(addr, ep.Service.Port),
				},
			},
			HealthStatus:        healthStatus,
			LoadBalancingWeight: makeUint32Value(weight),
		})
	}
	return &envoy.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []envoyendpoint.LocalityLbEndpoints{
			{
				LbEndpoints: es,
			},
		},
	}
}

func makeLoadAssignmentForDiscoveryChain(
	clusterName string,
	chain *structs.CompiledDiscoveryChain,
	overprovisioningFactor int,
	priorityEndpoints []structs.CheckServiceNodes,
) *envoy.ClusterLoadAssignment {
	cla := &envoy.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints:   make([]envoyendpoint.LocalityLbEndpoints, 0, len(priorityEndpoints)),
	}
	if overprovisioningFactor > 0 {
		cla.Policy = &envoy.ClusterLoadAssignment_Policy{
			OverprovisioningFactor: makeUint32Value(overprovisioningFactor),
		}
	}

	for priority, endpoints := range priorityEndpoints {
		es := make([]envoyendpoint.LbEndpoint, 0, len(endpoints))

		for _, ep := range endpoints {
			addr := ep.Service.Address
			if addr == "" {
				addr = ep.Node.Address
			}
			healthStatus := envoycore.HealthStatus_HEALTHY
			weight := 1
			if ep.Service.Weights != nil {
				weight = ep.Service.Weights.Passing
			}

			for _, chk := range ep.Checks {
				if chk.Status == api.HealthCritical {
					// This can't actually happen now because health always filters critical
					// but in the future it may not so set this correctly!
					healthStatus = envoycore.HealthStatus_UNHEALTHY
				}
				if chk.Status == api.HealthWarning && ep.Service.Weights != nil {
					weight = ep.Service.Weights.Warning
				}
			}
			// Make weights fit Envoy's limits. A zero weight means that either Warning
			// (likely) or Passing (weirdly) weight has been set to 0 effectively making
			// this instance unhealthy and should not be sent traffic.
			if weight < 1 {
				healthStatus = envoycore.HealthStatus_UNHEALTHY
				weight = 1
			}
			if weight > 128 {
				weight = 128
			}
			es = append(es, envoyendpoint.LbEndpoint{
				HostIdentifier: &envoyendpoint.LbEndpoint_Endpoint{
					Endpoint: &envoyendpoint.Endpoint{
						Address: makeAddressPtr(addr, ep.Service.Port),
					},
				},
				HealthStatus:        healthStatus,
				LoadBalancingWeight: makeUint32Value(weight),
			})
		}

		cla.Endpoints = append(cla.Endpoints, envoyendpoint.LocalityLbEndpoints{
			Priority:    uint32(priority),
			LbEndpoints: es,
		})
	}

	return cla
}
