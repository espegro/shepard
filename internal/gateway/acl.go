package gateway

import (
	"fmt"
	"net"
	"strings"
)

type clientACL struct {
	networks []*net.IPNet
}

func newClientACL(values []string) (*clientACL, error) {
	acl := &clientACL{networks: make([]*net.IPNet, 0, len(values))}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				return nil, fmt.Errorf("invalid client network %q", value)
			}
			if ipv4 := ip.To4(); ipv4 != nil {
				ip = ipv4
				value += "/32"
			} else {
				value += "/128"
			}
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("invalid client network %q: %w", value, err)
		}
		acl.networks = append(acl.networks, network)
	}
	return acl, nil
}

func (a *clientACL) allowed(remoteAddr string) bool {
	return a.contains(parseRemoteIP(remoteAddr))
}

func (a *clientACL) contains(ip net.IP) bool {
	if len(a.networks) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	for _, network := range a.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func parseRemoteIP(remoteAddr string) net.IP {
	host := remoteAddr
	if parsedHost, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	return net.ParseIP(host)
}

func forwardedClientIP(remoteAddr string, forwarded string, trusted *clientACL) net.IP {
	peer := parseRemoteIP(remoteAddr)
	if trusted == nil || !trusted.contains(peer) {
		return peer
	}
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			continue
		}
		if !trusted.contains(candidate) {
			return candidate
		}
	}
	return peer
}

func clientIPString(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}
