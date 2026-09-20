// proxy.go is a minimal inherit-only routing stub. The plugin-level explicit
// proxy feature (proxy-url config) was removed: all outbound requests now
// always follow the host's routing (host.http bridge, or the shared client on
// the direct fallback). The proxyState atomic and mode constants stay because
// host_bridge.go, oauth.go and panel.go branch on them; they are pinned to
// inherit so those branches always take the host path. configureProxy rejects
// any non-empty proxy-url so a stale config line fails loudly at reconfigure
// instead of being silently ignored.
package main

import (
	"errors"
	"net/http"
	"sync/atomic"
)

type proxyMode uint8

const (
	proxyModeInherit proxyMode = iota
	proxyModeExplicit
	proxyModeBlocked
)

type proxyRoutingState struct {
	mode   proxyMode
	client *http.Client
}

var proxyState atomic.Pointer[proxyRoutingState]

func init() {
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
}

func currentProxyState() *proxyRoutingState {
	return proxyState.Load()
}

// configureProxy accepts only an empty (unset) proxy-url. Any explicit proxy
// value is rejected: the plugin no longer carries its own proxy transport.
func configureProxy(raw string) error {
	if raw == "" {
		proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
		return nil
	}
	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	return errors.New("proxy-url is no longer supported by this plugin; remove the setting and rely on host routing")
}

func blockedProxyError() error {
	return errors.New("HTTP requests blocked: plugin-level proxy-url is not supported")
}
