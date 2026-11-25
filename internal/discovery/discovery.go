// Package discovery builds the narrow Traefik view exposed by Onebox's
// socketless proxy controller. The controller may inspect Docker, but the
// internet-facing proxy receives only this routing document.
package discovery

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Container is the deliberately small subset of Docker inspect data used to
// discover routes. Environment, mounts, host configuration, and the Docker API
// itself never cross into the generated document.
type Container struct {
	ID       string
	Created  time.Time
	Running  bool
	Health   string
	Labels   map[string]string
	Networks map[string]string
}

type Dynamic struct {
	HTTP *HTTP `json:"http,omitempty" yaml:"http,omitempty"`
	TCP  *TCP  `json:"tcp,omitempty" yaml:"tcp,omitempty"`
}

type HTTP struct {
	Routers  map[string]HTTPRouter  `json:"routers,omitempty" yaml:"routers,omitempty"`
	Services map[string]HTTPService `json:"services,omitempty" yaml:"services,omitempty"`
}

type TCP struct {
	Routers           map[string]TCPRouter           `json:"routers,omitempty" yaml:"routers,omitempty"`
	Services          map[string]TCPService          `json:"services,omitempty" yaml:"services,omitempty"`
	ServersTransports map[string]TCPServersTransport `json:"serversTransports,omitempty" yaml:"serversTransports,omitempty"`
}

// ProxyProtocolV2Transport is the one TCP servers transport discovery defines.
// A TCP service selects it with the serverstransport label, and Traefik then
// writes a PROXY protocol v2 header naming the client before the connection's
// first byte. Traefik v3.5.2 deprecated the per-service
// loadBalancer.proxyProtocol option in favour of this transport setting.
const ProxyProtocolV2Transport = "onebox-proxy-protocol-v2"

type TCPServersTransport struct {
	ProxyProtocol *ProxyProtocol `json:"proxyProtocol,omitempty" yaml:"proxyProtocol,omitempty"`
}

type ProxyProtocol struct {
	Version int `json:"version" yaml:"version"`
}

type HTTPRouter struct {
	Rule        string   `json:"rule" yaml:"rule"`
	EntryPoints []string `json:"entryPoints,omitempty" yaml:"entryPoints,omitempty"`
	Middlewares []string `json:"middlewares,omitempty" yaml:"middlewares,omitempty"`
	Service     string   `json:"service" yaml:"service"`
	TLS         *HTTPTLS `json:"tls,omitempty" yaml:"tls,omitempty"`
}

type HTTPTLS struct {
	CertResolver string      `json:"certResolver,omitempty" yaml:"certResolver,omitempty"`
	Domains      []TLSDomain `json:"domains,omitempty" yaml:"domains,omitempty"`
}

type TLSDomain struct {
	Main string   `json:"main" yaml:"main"`
	SANs []string `json:"sans,omitempty" yaml:"sans,omitempty"`
}

type TCPRouter struct {
	Rule        string   `json:"rule" yaml:"rule"`
	EntryPoints []string `json:"entryPoints,omitempty" yaml:"entryPoints,omitempty"`
	Middlewares []string `json:"middlewares,omitempty" yaml:"middlewares,omitempty"`
	Service     string   `json:"service" yaml:"service"`
	TLS         *TCPTLS  `json:"tls,omitempty" yaml:"tls,omitempty"`
}

type TCPTLS struct {
	Passthrough  bool   `json:"passthrough,omitempty" yaml:"passthrough,omitempty"`
	CertResolver string `json:"certResolver,omitempty" yaml:"certResolver,omitempty"`
}

type HTTPService struct {
	LoadBalancer HTTPLoadBalancer `json:"loadBalancer" yaml:"loadBalancer"`
}

type HTTPLoadBalancer struct {
	Servers []HTTPServer `json:"servers" yaml:"servers"`
}

type HTTPServer struct {
	URL string `json:"url" yaml:"url"`
}

type TCPService struct {
	LoadBalancer TCPLoadBalancer `json:"loadBalancer" yaml:"loadBalancer"`
}

type TCPLoadBalancer struct {
	Servers          []TCPServer `json:"servers" yaml:"servers"`
	ServersTransport string      `json:"serversTransport,omitempty" yaml:"serversTransport,omitempty"`
}

type TCPServer struct {
	Address string `json:"address" yaml:"address"`
}

type httpCandidate struct {
	created time.Time
	id      string
	router  HTTPRouter
}

type tcpCandidate struct {
	created time.Time
	id      string
	router  TCPRouter
}

// tcpBackend is one container's server for a TCP service, with the transport
// that container's labels ask for. The transport belongs to the service, not
// to a server, so the servers of one service must agree on it.
type tcpBackend struct {
	created   time.Time
	id        string
	address   string
	transport string
}

// Build converts healthy containers on network into Traefik's file-provider
// model. A container without a healthcheck is eligible once running; a
// container with one is eligible only while Docker reports it healthy. That is
// the Docker-provider behavior Onebox's rolling drain protocol relies on.
func Build(containers []Container, networkName string) (Dynamic, error) {
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	httpRouters := map[string]httpCandidate{}
	tcpRouters := map[string]tcpCandidate{}
	httpServers := map[string]map[string]struct{}{}
	tcpBackends := map[string][]tcpBackend{}

	for _, container := range containers {
		if !eligible(container) || !enabled(container.Labels) {
			continue
		}
		ip := container.Networks[networkName]
		if net.ParseIP(ip) == nil {
			continue
		}
		if err := collectHTTP(container, ip, httpRouters, httpServers); err != nil {
			return Dynamic{}, err
		}
		if err := collectTCP(container, ip, tcpRouters, tcpBackends); err != nil {
			return Dynamic{}, err
		}
	}

	out := Dynamic{}
	if len(httpRouters) > 0 || len(httpServers) > 0 {
		out.HTTP = &HTTP{Routers: map[string]HTTPRouter{}, Services: map[string]HTTPService{}}
		for name, candidate := range httpRouters {
			out.HTTP.Routers[name] = candidate.router
		}
		for name, servers := range httpServers {
			values := sortedSet(servers)
			items := make([]HTTPServer, 0, len(values))
			for _, value := range values {
				items = append(items, HTTPServer{URL: value})
			}
			out.HTTP.Services[name] = HTTPService{LoadBalancer: HTTPLoadBalancer{Servers: items}}
		}
	}
	if len(tcpRouters) > 0 || len(tcpBackends) > 0 {
		out.TCP = &TCP{Routers: map[string]TCPRouter{}, Services: map[string]TCPService{}}
		for name, candidate := range tcpRouters {
			out.TCP.Routers[name] = candidate.router
		}
		for name, backends := range tcpBackends {
			service := tcpService(backends)
			if service.LoadBalancer.ServersTransport == ProxyProtocolV2Transport {
				out.TCP.ServersTransports = map[string]TCPServersTransport{
					ProxyProtocolV2Transport: {ProxyProtocol: &ProxyProtocol{Version: 2}},
				}
			}
			out.TCP.Services[name] = service
		}
	}
	return out, nil
}

func eligible(container Container) bool {
	if !container.Running {
		return false
	}
	return container.Health == "" || container.Health == "healthy"
}

func enabled(labels map[string]string) bool {
	value, err := strconv.ParseBool(labels["traefik.enable"])
	return err == nil && value
}

func collectHTTP(container Container, ip string, routers map[string]httpCandidate, servers map[string]map[string]struct{}) error {
	for _, name := range routerNames(container.Labels, "http") {
		prefix := "traefik.http.routers." + name + "."
		service := container.Labels[prefix+"service"]
		rule := container.Labels[prefix+"rule"]
		if service == "" || rule == "" {
			continue
		}
		port, err := routePort(container.Labels, "http", service)
		if err != nil {
			return fmt.Errorf("container %s http router %s: %w", shortID(container.ID), name, err)
		}
		scheme := container.Labels["traefik.http.services."+service+".loadbalancer.server.scheme"]
		if scheme == "" {
			scheme = "http"
		}
		if scheme != "http" && scheme != "https" && scheme != "h2c" {
			return fmt.Errorf("container %s http service %s: unsupported scheme %q", shortID(container.ID), service, scheme)
		}
		addServer(servers, service, scheme+"://"+net.JoinHostPort(ip, strconv.Itoa(port)))
		router := HTTPRouter{
			Rule: rule, EntryPoints: splitList(container.Labels[prefix+"entrypoints"]),
			Middlewares: splitList(container.Labels[prefix+"middlewares"]), Service: service,
		}
		if truthy(container.Labels[prefix+"tls"]) {
			router.TLS = &HTTPTLS{CertResolver: container.Labels[prefix+"tls.certresolver"]}
			if main := container.Labels[prefix+"tls.domains[0].main"]; main != "" {
				router.TLS.Domains = []TLSDomain{{
					Main: main,
					SANs: splitList(container.Labels[prefix+"tls.domains[0].sans"]),
				}}
			}
		}
		candidate := httpCandidate{created: container.Created, id: container.ID, router: router}
		if current, exists := routers[name]; !exists || newer(candidate.created, candidate.id, current.created, current.id) {
			routers[name] = candidate
		}
	}
	return nil
}

// tcpService takes the transport from the newest backend, as routers are
// taken from the newest container, and keeps only the servers that agree with
// it. During a rolling switch that changes the transport, the old containers
// leave the service as soon as the first new one is healthy rather than being
// sent a PROXY header they do not expect, or sending none to one that does.
func tcpService(backends []tcpBackend) TCPService {
	newest := backends[0]
	for _, backend := range backends[1:] {
		if newer(backend.created, backend.id, newest.created, newest.id) {
			newest = backend
		}
	}
	addresses := map[string]struct{}{}
	for _, backend := range backends {
		if backend.transport == newest.transport {
			addresses[backend.address] = struct{}{}
		}
	}
	values := sortedSet(addresses)
	items := make([]TCPServer, 0, len(values))
	for _, value := range values {
		items = append(items, TCPServer{Address: value})
	}
	return TCPService{LoadBalancer: TCPLoadBalancer{Servers: items, ServersTransport: newest.transport}}
}

func collectTCP(container Container, ip string, routers map[string]tcpCandidate, backends map[string][]tcpBackend) error {
	for _, name := range routerNames(container.Labels, "tcp") {
		prefix := "traefik.tcp.routers." + name + "."
		service := container.Labels[prefix+"service"]
		rule := container.Labels[prefix+"rule"]
		if service == "" || rule == "" {
			continue
		}
		port, err := routePort(container.Labels, "tcp", service)
		if err != nil {
			return fmt.Errorf("container %s tcp router %s: %w", shortID(container.ID), name, err)
		}
		transport := container.Labels["traefik.tcp.services."+service+".loadbalancer.serverstransport"]
		if transport != "" && transport != ProxyProtocolV2Transport {
			return fmt.Errorf("container %s tcp service %s: unsupported servers transport %q", shortID(container.ID), service, transport)
		}
		backends[service] = append(backends[service], tcpBackend{
			created: container.Created, id: container.ID,
			address: net.JoinHostPort(ip, strconv.Itoa(port)), transport: transport,
		})
		router := TCPRouter{
			Rule: rule, EntryPoints: splitList(container.Labels[prefix+"entrypoints"]),
			Middlewares: splitList(container.Labels[prefix+"middlewares"]), Service: service,
		}
		if truthy(container.Labels[prefix+"tls"]) {
			router.TLS = &TCPTLS{
				Passthrough:  truthy(container.Labels[prefix+"tls.passthrough"]),
				CertResolver: container.Labels[prefix+"tls.certresolver"],
			}
		}
		candidate := tcpCandidate{created: container.Created, id: container.ID, router: router}
		if current, exists := routers[name]; !exists || newer(candidate.created, candidate.id, current.created, current.id) {
			routers[name] = candidate
		}
	}
	return nil
}

func routerNames(labels map[string]string, protocol string) []string {
	prefix := "traefik." + protocol + ".routers."
	set := map[string]struct{}{}
	for key := range labels {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		name, _, ok := strings.Cut(rest, ".")
		if ok && name != "" {
			set[name] = struct{}{}
		}
	}
	return sortedSet(set)
}

func routePort(labels map[string]string, protocol, service string) (int, error) {
	value := labels["traefik."+protocol+".services."+service+".loadbalancer.server.port"]
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid backend port %q", value)
	}
	return port, nil
}

func addServer(servers map[string]map[string]struct{}, service, address string) {
	if servers[service] == nil {
		servers[service] = map[string]struct{}{}
	}
	servers[service][address] = struct{}{}
}

func splitList(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func truthy(value string) bool {
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func newer(created time.Time, id string, currentCreated time.Time, currentID string) bool {
	return created.After(currentCreated) || (created.Equal(currentCreated) && id > currentID)
}

func sortedSet[T ~string](set map[T]struct{}) []T {
	out := make([]T, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
