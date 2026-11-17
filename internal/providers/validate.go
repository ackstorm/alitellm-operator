// SPDX-License-Identifier: MIT

package providers

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ValidateBaseURL is a cloud-metadata / loopback / link-local DENYLIST for
// ModelDiscovery spec.baseUrl — NOT a full SSRF guard.
//
// KubeAI deployments legitimately point baseUrl at in-cluster service DNS
// (*.svc) and private RFC1918 ranges, so a blanket internal-deny would
// break the operator's own supported use case. This function therefore
// only rejects the addresses with the highest blast radius — the cloud
// metadata endpoints (169.254.169.254, fd00:ec2::254), loopback, and
// link-local — and ACCEPTS every other host that passes the structural
// checks, including private and *.svc hosts.
//
// RESIDUAL RISK (by design): a namespaced user can still aim the operator's
// HTTP client at other internal/private services reachable from the
// operator pod. A host-allowlist is intentionally deferred. See
// CLAUDE.md "M-SEC1".
//
// Structural checks mirror litellm.ValidateEndpoint: http/https scheme, no
// userinfo/opaque/query/fragment, host required.
func ValidateBaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" || raw == "/" {
		return errors.New("empty baseUrl")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse baseUrl: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Opaque != "" {
		return errors.New("opaque baseUrl (use scheme://host[:port][/path])")
	}
	if u.User != nil {
		return errors.New("userinfo not allowed in baseUrl")
	}
	if u.RawQuery != "" {
		return errors.New("query string not allowed in baseUrl")
	}
	if u.Fragment != "" {
		return errors.New("fragment not allowed in baseUrl")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("host required")
	}
	if portStr := u.Port(); portStr != "" {
		p, perr := strconv.Atoi(portStr)
		if perr != nil || p < 1 || p > 65535 {
			return errors.New("invalid port (must be 1-65535)")
		}
	}
	if err := deniedHost(host); err != nil {
		return fmt.Errorf("baseUrl %w", err)
	}
	return nil
}

// deniedHost rejects IP literals and the localhost names. Other DNS names
// (e.g. *.svc) are NOT resolved — the denylist targets metadata/loopback/
// link-local addresses a caller would hardcode; DNS-based rebinding is out of
// scope (documented residual risk).
func deniedHost(host string) error {
	if h := strings.TrimSuffix(strings.ToLower(host), "."); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("host %q is loopback (denied)", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return fmt.Errorf("host %q is loopback (denied)", host)
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("host %q is link-local (denied)", host)
		}
		if isCloudMetadataIP(ip) {
			return fmt.Errorf("host %q is a cloud metadata address (denied)", host)
		}
	}
	return nil
}

// CheckRedirect is the redirect policy for discovery HTTP clients: a redirect
// may not reach a host ValidateBaseURL denies.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return deniedHost(req.URL.Hostname())
}

// isCloudMetadataIP reports whether ip is a well-known cloud instance
// metadata service address. 169.254.169.254 is already covered by the
// link-local check, but is listed explicitly for clarity and to catch the
// IPv6 metadata address fd00:ec2::254 (a unique-local address, NOT
// link-local).
func isCloudMetadataIP(ip net.IP) bool {
	for _, m := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if ip.Equal(net.ParseIP(m)) {
			return true
		}
	}
	return false
}
