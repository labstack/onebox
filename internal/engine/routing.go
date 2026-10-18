package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/discovery"
	"github.com/labstack/onebox/internal/proxy"
	"gopkg.in/yaml.v3"
)

const routePublishBudget = 30 * time.Second

func (e *Engine) routeGated(role app.Workload) bool {
	return e.Spec.Proxy.Managed && len(role.NormalisedRoutes()) > 0
}

func routedIn(doc discovery.Dynamic, n app.Names, workload string, role app.Workload, address string) bool {
	for i, route := range role.NormalisedRoutes() {
		if !routeContains(doc, n.ProxyServiceFor(workload, i), route.Protocol, address) {
			return false
		}
	}
	return true
}

// withdrawnFromAll checks every published service, including routes absent from the new role.
func withdrawnFromAll(doc discovery.Dynamic, address string) bool {
	if doc.HTTP != nil {
		for service := range doc.HTTP.Services {
			if routeContains(doc, service, "http", address) {
				return false
			}
		}
	}
	if doc.TCP != nil {
		for service := range doc.TCP.Services {
			if routeContains(doc, service, "tcp", address) {
				return false
			}
		}
	}
	return true
}

func routeContains(doc discovery.Dynamic, service, protocol, address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	if protocol == "tcp" {
		if doc.TCP == nil {
			return false
		}
		for _, server := range doc.TCP.Services[service].LoadBalancer.Servers {
			host, _, err := net.SplitHostPort(server.Address)
			if err == nil && ip.Equal(net.ParseIP(host)) {
				return true
			}
		}
	} else {
		if doc.HTTP == nil {
			return false
		}
		for _, server := range doc.HTTP.Services[service].LoadBalancer.Servers {
			endpoint, err := url.Parse(server.URL)
			if err == nil && ip.Equal(net.ParseIP(endpoint.Hostname())) {
				return true
			}
		}
	}
	return false
}

func (e *Engine) routeAddress(ctx context.Context, id string) (string, error) {
	template := fmt.Sprintf("{{with index .NetworkSettings.Networks %q}}{{.IPAddress}} {{.GlobalIPv6Address}}{{end}}", e.Spec.Proxy.Network)
	result, err := e.T.Run(ctx, "docker inspect -f "+q(template)+" "+q(id))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("inspect route address: exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	ipv4, ipv6, _ := strings.Cut(strings.TrimRight(result.Stdout, "\r\n"), " ")
	address := discovery.EndpointAddress(ipv4, strings.TrimSpace(ipv6))
	return address, nil
}

func (e *Engine) readPublishedRoutes(ctx context.Context) (discovery.Dynamic, error) {
	var doc discovery.Dynamic
	result, err := e.T.Run(ctx, "cat "+q(proxy.HostPaths(e.names()).Dynamic+"/onebox.yml"))
	if err != nil {
		return doc, err
	}
	if err := ctx.Err(); err != nil {
		return doc, err
	}
	if result.ExitCode != 0 {
		return doc, fmt.Errorf("read published routes: exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	decoder := yaml.NewDecoder(strings.NewReader(result.Stdout))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return doc, fmt.Errorf("read published routes: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Tag == "!!null" {
		return doc, fmt.Errorf("read published routes: empty YAML document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return doc, fmt.Errorf("read published routes: %w", err)
		}
		return doc, fmt.Errorf("read published routes: expected one YAML document")
	}
	if err := document.Decode(&doc); err != nil {
		return discovery.Dynamic{}, fmt.Errorf("read published routes: %w", err)
	}
	return doc, nil
}

func (e *Engine) awaitRouting(ctx context.Context, workload string, role app.Workload, id string, routed bool, pollEvery time.Duration) error {
	address, err := e.routeAddress(ctx, id)
	if err != nil {
		return err
	}
	if address == "" {
		if routed {
			return fmt.Errorf("container %s has no address on network %s", id, e.Spec.Proxy.Network)
		}
		return nil
	}
	deadline := e.Opts.Now().Add(routePublishBudget)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, err := e.readPublishedRoutes(ctx)
		if err != nil {
			lastErr = err
		} else if routed && routedIn(doc, e.names(), workload, role, address) || !routed && withdrawnFromAll(doc, address) {
			return nil
		}
		if !e.Opts.Now().Before(deadline) {
			message := fmt.Sprintf("discovery did not publish container %s for workload %s; check docker logs onebox-discovery", id, workload)
			if !routed {
				message = fmt.Sprintf("discovery still routes to container %s for workload %s; fix discovery and run ob resume", id, workload)
			}
			if lastErr != nil {
				return fmt.Errorf("%s: %w", message, lastErr)
			}
			return fmt.Errorf("%s", message)
		}
		interval := min(pollEvery, deadline.Sub(e.Opts.Now()))
		if err := e.Opts.Wait(ctx, interval); err != nil {
			return err
		}
	}
}
