package proxy

import "github.com/labstack/onebox/internal/app"

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

// RenderCompose emits the proxy's own compose file. Deliberately a template,
// not compose-go construction: the shape IS the contract and a reviewer should
// be able to read it whole.
func RenderCompose(image, network string, hasEnv bool, entrypoints map[string]app.ProxyEntrypoint) []byte {
	return RenderComposeForApp(image, DiscoveryImage(""), "onebox", network, hasEnv, entrypoints)
}

func renderStaticConfig(entrypoints map[string]app.ProxyEntrypoint) []byte {
	return renderStaticConfigWithDNS(entrypoints, nil)
}
