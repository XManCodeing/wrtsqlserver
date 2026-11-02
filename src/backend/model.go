package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

// Proxy is a stored SOCKS5 endpoint. Password is never serialized by API views.
type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

// ProxyInput represents validated administrator input, including explicit credential removal.
type ProxyInput struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clearPassword"`
}

// Mapping defines an IPv4 listener and a remotely resolved SOCKS5 destination.
type Mapping struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	ListenHost     string   `json:"listenHost"`
	ListenPort     int      `json:"listenPort"`
	TargetHost     string   `json:"targetHost"`
	TargetPort     int      `json:"targetPort"`
	ProxyID        string   `json:"proxyId"`
	Enabled        bool     `json:"enabled"`
	MaxConnections int      `json:"maxConnections"`
	AllowedCIDRs   []string `json:"allowedCIDRs"`
}

// Config is the encrypted-on-disk configuration; collections always serialize as arrays.
type Config struct {
	Proxies  []Proxy   `json:"proxies"`
	Mappings []Mapping `json:"mappings"`
}

var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// validHost returns whether host is an IP literal or an ASCII DNS name, without a port.
func validHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if !hostnamePattern.MatchString(label) {
			return false
		}
	}
	return true
}

// validName validates a nonempty display name and rejects control characters.
func validName(name string) bool {
	if len(strings.TrimSpace(name)) == 0 || len(name) > 100 {
		return false
	}
	return !strings.ContainsAny(name, "\x00\r\n\t")
}

// validateProxy checks protocol bounds on an input endpoint; returns a safe validation error.
func validateProxy(input ProxyInput) error {
	if !validName(input.Name) {
		return errors.New("name must be 1–100 bytes without control characters")
	}
	if !validHost(input.Host) {
		return errors.New("host must be an IP address or DNS name")
	}
	if input.Port < 1 || input.Port > 65535 {
		return errors.New("port must be 1–65535")
	}
	if len(input.Username) > 255 || len(input.Password) > 255 {
		return errors.New("SOCKS5 credentials must not exceed 255 bytes")
	}
	if input.ClearPassword && input.Password != "" {
		return errors.New("password and clearPassword cannot both be supplied")
	}
	return nil
}

// validateProxyCredentials validates final credentials after password preservation/clearing.
// Both empty means unauthenticated SOCKS5; either nonempty requires the complete pair.
func validateProxyCredentials(proxy Proxy) error {
	if (proxy.Username == "") != (proxy.Password == "") {
		return errors.New("username and password must both be provided or both cleared")
	}
	return nil
}

// validateMapping validates IPv4 binding, bounded resources, ACL and referential integrity.
// cfg and apiAddr supply the existing configuration and management listener address.
func validateMapping(mapping Mapping, cfg Config, apiAddr string) error {
	if !validName(mapping.Name) {
		return errors.New("name must be 1–100 bytes without control characters")
	}
	address, err := netip.ParseAddr(mapping.ListenHost)
	if err != nil || !address.Is4() || !(address.IsPrivate() || address.IsLoopback() || address.IsUnspecified()) {
		return errors.New("listenHost must be explicit private/loopback IPv4 or 0.0.0.0")
	}
	if mapping.ListenPort < 1 || mapping.ListenPort > 65535 || mapping.TargetPort < 1 || mapping.TargetPort > 65535 {
		return errors.New("ports must be 1–65535")
	}
	if !validHost(mapping.TargetHost) {
		return errors.New("targetHost must be an IP address or DNS name")
	}
	if mapping.MaxConnections < 1 || mapping.MaxConnections > 4096 {
		return errors.New("maxConnections must be 1–4096")
	}
	if len(mapping.AllowedCIDRs) < 1 || len(mapping.AllowedCIDRs) > 64 {
		return errors.New("allowedCIDRs requires 1–64 IPv4 networks")
	}
	for _, raw := range mapping.AllowedCIDRs {
		prefix, parseErr := netip.ParsePrefix(raw)
		if parseErr != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 {
			return errors.New("allowedCIDRs must contain IPv4 CIDRs, never 0.0.0.0/0")
		}
	}
	_, apiPort, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return errors.New("invalid management listen address")
	}
	if apiPort == fmt.Sprint(mapping.ListenPort) {
		return errors.New("mapping port conflicts with management port")
	}
	found := false
	for _, proxy := range cfg.Proxies {
		if proxy.ID == mapping.ProxyID {
			found = true
		}
	}
	if !found {
		return errors.New("proxyId does not exist")
	}
	for _, existing := range cfg.Mappings {
		if existing.ID != mapping.ID && existing.ListenPort == mapping.ListenPort &&
			(existing.ListenHost == mapping.ListenHost || existing.ListenHost == "0.0.0.0" || mapping.ListenHost == "0.0.0.0") {
			return errors.New("mapping listener conflicts with another mapping")
		}
	}
	return nil
}

// cloneConfig returns an independent configuration snapshot, including ACL slices.
func cloneConfig(cfg Config) Config {
	copied := Config{Proxies: append([]Proxy{}, cfg.Proxies...), Mappings: append([]Mapping{}, cfg.Mappings...)}
	for index := range copied.Mappings {
		copied.Mappings[index].AllowedCIDRs = append([]string{}, copied.Mappings[index].AllowedCIDRs...)
	}
	return copied
}
