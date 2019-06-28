package agent

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/consul/agent/structs"
)

// TODO(rb): clean this up before exposing for non-debug purposes
func (s *HTTPServer) ReadDiscoveryChain(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.DiscoveryChainRequest
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	service := strings.TrimPrefix(req.URL.Path, "/v1/discovery/chain/")
	if service == "" {
		return nil, BadRequestError{Reason: "Missing service name"}
	}

	args.Name = service

	var reply structs.DiscoveryChainResponse
	if err := s.agent.RPC("ConfigEntry.ReadDiscoveryChain", &args, &reply); err != nil {
		return nil, err
	}
	setMeta(resp, &reply.QueryMeta)

	if reply.Chain == nil {
		return nil, NotFoundError{
			Reason: fmt.Sprintf("Discovery chain not found for %q", service),
		}
	}

	return reply.Chain, nil

}
