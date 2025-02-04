// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/types"

	meshconfig "istio.io/api/mesh/v1alpha1"

	"istio.io/istio/pkg/config/labels"
)

// Environment provides an aggregate environmental API for Pilot
type Environment struct {
	// Discovery interface for listing services and instances.
	ServiceDiscovery

	// Config interface for listing routing rules
	IstioConfigStore

	// Mesh is the mesh config (to be merged into the config store)
	Mesh *meshconfig.MeshConfig

	// PushContext holds informations during push generation. It is reset on config change, at the beginning
	// of the pushAll. It will hold all errors and stats and possibly caches needed during the entire cache computation.
	// DO NOT USE EXCEPT FOR TESTS AND HANDLING OF NEW CONNECTIONS.
	// ALL USE DURING A PUSH SHOULD USE THE ONE CREATED AT THE
	// START OF THE PUSH, THE GLOBAL ONE MAY CHANGE AND REFLECT A DIFFERENT
	// CONFIG AND PUSH
	PushContext *PushContext

	// MeshNetworks (loaded from a config map) provides information about the
	// set of networks inside a mesh and how to route to endpoints in each
	// network. Each network provides information about the endpoints in a
	// routable L3 network. A single routable L3 network can have one or more
	// service registries.
	MeshNetworks *meshconfig.MeshNetworks
}

// Proxy contains information about an specific instance of a proxy (envoy sidecar, gateway,
// etc). The Proxy is initialized when a sidecar connects to Pilot, and populated from
// 'node' info in the protocol as well as data extracted from registries.
//
// In current Istio implementation nodes use a 4-parts '~' delimited ID.
// Type~IPAddress~ID~Domain
type Proxy struct {
	// ClusterID specifies the cluster where the proxy resides.
	// TODO: clarify if this is needed in the new 'network' model, likely needs to
	// be renamed to 'network'
	ClusterID string

	// Type specifies the node type. First part of the ID.
	Type NodeType

	// IPAddresses is the IP addresses of the proxy used to identify it and its
	// co-located service instances. Example: "10.60.1.6". In some cases, the host
	// where the poxy and service instances reside may have more than one IP address
	IPAddresses []string

	// ID is the unique platform-specific sidecar proxy ID. For k8s it is the pod ID and
	// namespace.
	ID string

	// Locality is the location of where Envoy proxy runs. This is extracted from
	// the registry where possible. If the registry doesn't provide a locality for the
	// proxy it will use the one sent via ADS that can be configured in the Envoy bootstrap
	Locality *core.Locality

	// DNSDomain defines the DNS domain suffix for short hostnames (e.g.
	// "default.svc.cluster.local")
	DNSDomain string

	// ConfigNamespace defines the namespace where this proxy resides
	// for the purposes of network scoping.
	// NOTE: DO NOT USE THIS FIELD TO CONSTRUCT DNS NAMES
	ConfigNamespace string

	// Metadata key-value pairs extending the Node identifier
	Metadata map[string]string

	// the sidecarScope associated with the proxy
	SidecarScope *SidecarScope

	// The merged gateways associated with the proxy if this is a Router
	MergedGateway *MergedGateway

	// service instances associated with the proxy
	ServiceInstances []*ServiceInstance

	// labels associated with the workload
	WorkloadLabels labels.Collection

	// Istio version associated with the Proxy
	IstioVersion *IstioVersion
}

var (
	istioVersionRegexp = regexp.MustCompile(`^([1-9]+)\.([0-9]+)(\.([0-9]+))?`)
)

// IstioVersion encodes the Istio version of the proxy. This is a low key way to
// do semver style comparisons and generate the appropriate envoy config
type IstioVersion struct {
	Major int
	Minor int
	Patch int
}

var (
	MaxIstioVersion = &IstioVersion{Major: 65535, Minor: 65535, Patch: 65535}
)

// Compare returns -1/0/1 if version is less than, equal or greater than inv
// To compare only on major, call this function with { X, -1, -1}.
// to compare only on major & minor, call this function with {X, Y, -1}.
func (pversion *IstioVersion) Compare(inv *IstioVersion) int {
	if pversion.Major > inv.Major {
		return 1
	} else if pversion.Major < inv.Major {
		return -1
	}

	// check minors
	if inv.Minor > -1 {
		if pversion.Minor > inv.Minor {
			return 1
		} else if pversion.Minor < inv.Minor {
			return -1
		}
		// check patch
		if inv.Patch > -1 {
			if pversion.Patch > inv.Patch {
				return 1
			} else if pversion.Patch < inv.Patch {
				return -1
			}
		}
	}
	return 0
}

// NodeType decides the responsibility of the proxy serves in the mesh
type NodeType string

const (
	// SidecarProxy type is used for sidecar proxies in the application containers
	SidecarProxy NodeType = "sidecar"

	// Router type is used for standalone proxies acting as L7/L4 routers
	Router NodeType = "router"

	// AllPortsLiteral is the string value indicating all ports
	AllPortsLiteral = "*"
)

// IsApplicationNodeType verifies that the NodeType is one of the declared constants in the model
func IsApplicationNodeType(nType NodeType) bool {
	switch nType {
	case SidecarProxy, Router:
		return true
	default:
		return false
	}
}

// ServiceNode encodes the proxy node attributes into a URI-acceptable string
func (node *Proxy) ServiceNode() string {
	ip := ""
	if len(node.IPAddresses) > 0 {
		ip = node.IPAddresses[0]
	}
	return strings.Join([]string{
		string(node.Type), ip, node.ID, node.DNSDomain,
	}, serviceNodeSeparator)

}

// GetIstioVersion returns the Istio version of the proxy, and whether it is present
func (node *Proxy) GetIstioVersion() (string, bool) {
	version, found := node.Metadata[NodeMetadataIstioVersion]
	return version, found
}

// RouterMode decides the behavior of Istio Gateway (normal or sni-dnat)
type RouterMode string

const (
	// StandardRouter is the normal gateway mode
	StandardRouter RouterMode = "standard"

	// SniDnatRouter is used for bridging two networks
	SniDnatRouter RouterMode = "sni-dnat"
)

// GetRouterMode returns the operating mode associated with the router.
// Assumes that the proxy is of type Router
func (node *Proxy) GetRouterMode() RouterMode {
	if modestr, found := node.Metadata[NodeMetadataRouterMode]; found {
		if RouterMode(modestr) == SniDnatRouter {
			return SniDnatRouter
		}
	}
	return StandardRouter
}

// SetSidecarScope identifies the sidecar scope object associated with this
// proxy and updates the proxy Node. This is a convenience hack so that
// callers can simply call push.Services(node) while the implementation of
// push.Services can return the set of services from the proxyNode's
// sidecar scope or from the push context's set of global services. Similar
// logic applies to push.VirtualServices and push.DestinationRule. The
// short cut here is useful only for CDS and parts of RDS generation code.
//
// Listener generation code will still use the SidecarScope object directly
// as it needs the set of services for each listener port.
func (node *Proxy) SetSidecarScope(ps *PushContext) {
	if node.Type == SidecarProxy {
		node.SidecarScope = ps.getSidecarScope(node, node.WorkloadLabels)
	} else {
		// Gateways should just have a default scope with egress: */*
		node.SidecarScope = DefaultSidecarScopeForNamespace(ps, node.ConfigNamespace)
	}

}

// SetGatewaysForProxy merges the Gateway objects associated with this
// proxy and caches the merged object in the proxy Node. This is a convenience hack so that
// callers can simply call push.MergedGateways(node) instead of having to
// fetch all the gateways and invoke the merge call in multiple places (lds/rds).
func (node *Proxy) SetGatewaysForProxy(ps *PushContext) {
	if node.Type != Router {
		return
	}
	node.MergedGateway = ps.mergeGateways(node)
}

func (node *Proxy) SetServiceInstances(env *Environment) error {
	instances, err := env.GetProxyServiceInstances(node)
	if err != nil {
		log.Errorf("failed to get service proxy service instances: %v", err)
		return err
	}

	node.ServiceInstances = instances
	return nil
}

// SetWorkloadLabels will reset the proxy.WorkloadLabels if `force` = true,
// otherwise only set it when it is nil.
func (node *Proxy) SetWorkloadLabels(env *Environment, force bool) error {
	// The WorkloadLabels is already parsed from Node metadata["LABELS"]
	// Or updated in DiscoveryServer.WorkloadUpdate.
	if node.WorkloadLabels != nil {
		return nil
	}

	l, err := env.GetProxyWorkloadLabels(node)
	if err != nil {
		log.Errorf("failed to get service proxy labels: %v", err)
		return err
	}

	node.WorkloadLabels = l
	return nil
}

// UnnamedNetwork is the default network that proxies in the mesh
// get when they don't request a specific network view.
const UnnamedNetwork = ""

// GetNetworkView returns the networks that the proxy requested.
// When sending EDS/CDS-with-dns-endpoints, Pilot will only send
// endpoints corresponding to the networks that the proxy wants to see.
// If not set, we assume that the proxy wants to see endpoints from the default
// unnamed network.
func GetNetworkView(node *Proxy) map[string]bool {
	if node == nil {
		return map[string]bool{UnnamedNetwork: true}
	}

	nmap := make(map[string]bool)
	if networks, found := node.Metadata[NodeMetadataRequestedNetworkView]; found {
		for _, n := range strings.Split(networks, ",") {
			nmap[n] = true
		}
	} else {
		// Proxy sees endpoints from the default unnamed network only
		nmap[UnnamedNetwork] = true
	}
	return nmap
}

// ParseMetadata parses the opaque Metadata from an Envoy Node into string key-value pairs.
// Any non-string values are ignored.
func ParseMetadata(metadata *types.Struct) map[string]string {
	if metadata == nil {
		return nil
	}
	fields := metadata.GetFields()
	res := make(map[string]string, len(fields))
	for k, v := range fields {
		switch s := v.GetKind().(type) {
		case *types.Value_StringValue:
			res[k] = s.StringValue
		default:
			// Some fields are not simple strings, dump these to json strings.
			// TODO: convert metadata to a properly typed struct rather than map[string]string
			j, err := (&jsonpb.Marshaler{}).MarshalToString(v)
			if err != nil {
				log.Warnf("failed to unmarshal metadata field %v with value %v: %v", k, v, err)
				continue
			}
			res[k] = j
		}
	}
	if len(res) == 0 {
		res = nil
	}
	return res
}

// ParseServiceNodeWithMetadata parse the Envoy Node from the string generated by ServiceNode
// function and the metadata.
func ParseServiceNodeWithMetadata(s string, metadata map[string]string) (*Proxy, error) {
	parts := strings.Split(s, serviceNodeSeparator)
	out := &Proxy{
		Metadata: metadata,
	}

	if len(parts) != 4 {
		return out, fmt.Errorf("missing parts in the service node %q", s)
	}

	out.Type = NodeType(parts[0])

	if !IsApplicationNodeType(out.Type) {
		return out, fmt.Errorf("invalid node type (valid types: sidecar, router in the service node %q", s)
	}

	// Get all IP Addresses from Metadata
	if ipstr, found := metadata[NodeMetadataInstanceIPs]; found {
		ipAddresses, err := parseIPAddresses(ipstr)
		if err == nil {
			out.IPAddresses = ipAddresses
		} else if isValidIPAddress(parts[1]) {
			//Fail back, use IP from node id
			out.IPAddresses = append(out.IPAddresses, parts[1])
		}
	} else if isValidIPAddress(parts[1]) {
		// Get IP from node id, it's only for backward-compatible, IP should come from metadata
		out.IPAddresses = append(out.IPAddresses, parts[1])
	}

	// Does query from ingress or router have to carry valid IP address?
	if len(out.IPAddresses) == 0 {
		return out, fmt.Errorf("no valid IP address in the service node id or metadata")
	}

	out.ID = parts[2]
	out.DNSDomain = parts[3]
	out.IstioVersion = ParseIstioVersion(metadata[NodeMetadataIstioVersion])

	if data, ok := metadata[NodeMetadataLabels]; ok {
		var nodeLabels map[string]string
		if err := json.Unmarshal([]byte(data), &nodeLabels); err != nil {
			log.Warnf("invalid node label %s: %v", data, err)
		}
		if len(nodeLabels) > 0 {
			out.WorkloadLabels = labels.Collection{nodeLabels}
		}
	}
	return out, nil
}

// ParseIstioVersion parses a version string and returns IstioVersion struct
func ParseIstioVersion(ver string) *IstioVersion {
	if strings.HasPrefix(ver, "master-") {
		// This proxy is from a master branch build. Assume latest version
		return MaxIstioVersion
	}

	// strip the release- prefix if any and extract the version string
	ver = istioVersionRegexp.FindString(strings.TrimPrefix(ver, "release-"))

	if ver == "" {
		// return very large values assuming latest version
		return MaxIstioVersion
	}

	parts := strings.Split(ver, ".")
	// we are guaranteed to have atleast major and minor based on the regex
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	patch := 0
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}
	return &IstioVersion{Major: major, Minor: minor, Patch: patch}
}

// GetOrDefaultFromMap returns either the value found for key or the default value if the map is nil
// or does not contain the key. Useful when retrieving node metadata fields.
func GetOrDefaultFromMap(stringMap map[string]string, key, defaultVal string) string {
	if stringMap == nil {
		return defaultVal
	}
	if valFromMap, ok := stringMap[key]; ok {
		return valFromMap
	}
	return defaultVal
}

// GetProxyConfigNamespace extracts the namespace associated with the proxy
// from the proxy metadata or the proxy ID
func GetProxyConfigNamespace(proxy *Proxy) string {
	if proxy == nil {
		return ""
	}

	// First look for ISTIO_META_CONFIG_NAMESPACE
	// All newer proxies (from Istio 1.1 onwards) are supposed to supply this
	if configNamespace, found := proxy.Metadata[NodeMetadataConfigNamespace]; found {
		return configNamespace
	}

	// if not found, for backward compatibility, extract the namespace from
	// the proxy domain. this is a k8s specific hack and should be enabled
	parts := strings.Split(proxy.DNSDomain, ".")
	if len(parts) > 1 { // k8s will have namespace.<domain>
		return parts[0]
	}

	return ""
}

const (
	serviceNodeSeparator = "~"

	IstioIncludeInboundPorts = "INCLUDE_INBOUND_PORTS"
)

// ParsePort extracts port number from a valid proxy address
func ParsePort(addr string) int {
	port, err := strconv.Atoi(addr[strings.Index(addr, ":")+1:])
	if err != nil {
		log.Warna(err)
	}

	return port
}

// parseIPAddresses extracts IPs from a string
func parseIPAddresses(s string) ([]string, error) {
	ipAddresses := strings.Split(s, ",")
	if len(ipAddresses) == 0 {
		return ipAddresses, fmt.Errorf("no valid IP address")
	}
	for _, ipAddress := range ipAddresses {
		if !isValidIPAddress(ipAddress) {
			return ipAddresses, fmt.Errorf("invalid IP address %q", ipAddress)
		}
	}
	return ipAddresses, nil
}

// Tell whether the given IP address is valid or not
func isValidIPAddress(ip string) bool {
	return net.ParseIP(ip) != nil
}

// Pile all node metadata constants here
const (
	// NodeMetadataIstioVersion specifies the Istio version associated with the proxy
	NodeMetadataIstioVersion = "ISTIO_VERSION"

	// NodeMetadataNetwork defines the network the node belongs to. It is an optional metadata,
	// set at injection time. When set, the Endpoints returned to a note and not on same network
	// will be replaced with the gateway defined in the settings.
	NodeMetadataNetwork = "NETWORK"

	// NodeMetadataNetwork defines the cluster the node belongs to.
	NodeMetadataClusterID = "CLUSTER_ID"

	// NodeMetadataInterceptionMode is the name of the metadata variable that carries info about
	// traffic interception mode at the proxy
	NodeMetadataInterceptionMode = "INTERCEPTION_MODE"

	// NodeMetadataHTTP10 indicates the application behind the sidecar is making outbound http requests with HTTP/1.0
	// protocol. It will enable the "AcceptHttp_10" option on the http options for outbound HTTP listeners.
	// Alpha in 1.1, based on feedback may be turned into an API or change. Set to "1" to enable.
	NodeMetadataHTTP10 = "HTTP10"

	// NodeMetadataConfigNamespace is the name of the metadata variable that carries info about
	// the config namespace associated with the proxy
	NodeMetadataConfigNamespace = "CONFIG_NAMESPACE"

	// NodeMetadataRequestedNetworkView specifies the networks that the proxy wants to see
	NodeMetadataRequestedNetworkView = "REQUESTED_NETWORK_VIEW"

	// NodeMetadataRouterMode indicates whether the proxy is functioning as a SNI-DNAT router
	// processing the AUTO_PASSTHROUGH gateway servers
	NodeMetadataRouterMode = "ROUTER_MODE"

	// NodeMetadataInstanceIPs is the set of IPs attached to this proxy
	NodeMetadataInstanceIPs = "INSTANCE_IPS"

	// NodeMetadataSdsEnabled specifies whether SDS is enabled.
	NodeMetadataSdsEnabled = "ISTIO_META_SDS"

	// NodeMetadataSdsEnabled specifies whether trustworthy jwt is used to request key/cert through SDS>
	NodeMetadataSdsTrustJwt = "ISTIO_META_TRUSTJWT"

	// NodeMetadataSdsTokenPath specifies the path of the SDS token used by the Envoy proxy.
	// If not set, Pilot uses the default SDS token path.
	NodeMetadataSdsTokenPath = "SDS_TOKEN_PATH"

	// NodeMetadataMeshID specifies the mesh ID environment variable.
	NodeMetadataMeshID = "MESH_ID"

	// NodeMetadataTLSServerCertChain is the absolute path to server cert-chain file
	NodeMetadataTLSServerCertChain = "TLS_SERVER_CERT_CHAIN"

	// NodeMetadataTLSServerKey is the absolute path to server private key file
	NodeMetadataTLSServerKey = "TLS_SERVER_KEY"

	// NodeMetadataTLSServerRootCert is the absolute path to server root cert file
	NodeMetadataTLSServerRootCert = "TLS_SERVER_ROOT_CERT"

	// NodeMetadataTLSClientCertChain is the absolute path to client cert-chain file
	NodeMetadataTLSClientCertChain = "TLS_CLIENT_CERT_CHAIN"

	// NodeMetadataTLSClientKey is the absolute path to client private key file
	NodeMetadataTLSClientKey = "TLS_CLIENT_KEY"

	// NodeMetadataTLSClientRootCert is the absolute path to client root cert file
	NodeMetadataTLSClientRootCert = "TLS_CLIENT_ROOT_CERT"

	// NodeMetadataIdleTimeout specifies the idle timeout for the proxy, in duration format (10s).
	// If not set, no timeout is set.
	NodeMetadataIdleTimeout = "IDLE_TIMEOUT"

	// NodeMetadataPodPorts the ports on a pod. This is used to lookup named ports.
	NodeMetadataPodPorts = "POD_PORTS"

	// NodeMetadataCanonicalTelemetryService specifies the service name to use for all node telemetry.
	NodeMetadataCanonicalTelemetryService = "CANONICAL_TELEMETRY_SERVICE"

	// NodeMetadataLabels specifies the set of workload instance (ex: k8s pod) labels associated with this node.
	NodeMetadataLabels = "LABELS"

	// NodeMetadataWorkloadName specifies the name of the workload represented by this node.
	NodeMetadataWorkloadName = "WORKLOAD_NAME"

	// NodeMetadataOwner specifies the workload owner (opaque string). Typically, this is the owning controller of
	// of the workload instance (ex: k8s deployment for a k8s pod).
	NodeMetadataOwner = "OWNER"

	// NodeMetadataServiceAccount specifies the service account which is running the workload.
	NodeMetadataServiceAccount = "SERVICE_ACCOUNT"

	// NodeMetadataPlatformMetadata contains any platform specific metadata
	NodeMetadataPlatformMetadata = "PLATFORM_METADATA"

	// NodeMetadataInstanceName is the short name for the workload instance (ex: pod name)
	NodeMetadataInstanceName = "NAME" // replaces POD_NAME

	// NodeMetadataNamespace is the namespace in which the workload instance is running.
	NodeMetadataNamespace = "NAMESPACE" // replaces CONFIG_NAMESPACE

	// NodeMetadataExchangeKeys specifies a list of metadata keys that should be used for Node Metadata Exchange.
	// The list is comma-separated.
	NodeMetadataExchangeKeys = "EXCHANGE_KEYS"
)

// TrafficInterceptionMode indicates how traffic to/from the workload is captured and
// sent to Envoy. This should not be confused with the CaptureMode in the API that indicates
// how the user wants traffic to be intercepted for the listener. TrafficInterceptionMode is
// always derived from the Proxy metadata
type TrafficInterceptionMode string

const (
	// InterceptionNone indicates that the workload is not using IPtables for traffic interception
	InterceptionNone TrafficInterceptionMode = "NONE"

	// InterceptionTproxy implies traffic intercepted by IPtables with TPROXY mode
	InterceptionTproxy TrafficInterceptionMode = "TPROXY"

	// InterceptionRedirect implies traffic intercepted by IPtables with REDIRECT mode
	// This is our default mode
	InterceptionRedirect TrafficInterceptionMode = "REDIRECT"
)

// GetInterceptionMode extracts the interception mode associated with the proxy
// from the proxy metadata
func (node *Proxy) GetInterceptionMode() TrafficInterceptionMode {
	if node == nil {
		return InterceptionRedirect
	}

	switch node.Metadata[NodeMetadataInterceptionMode] {
	case "TPROXY":
		return InterceptionTproxy
	case "REDIRECT":
		return InterceptionRedirect
	case "NONE":
		return InterceptionNone
	}

	return InterceptionRedirect
}
