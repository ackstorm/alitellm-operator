// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
)

// ValidateEndpoint rejects values that NewClient would otherwise accept
// silently. The CRD Pattern + CEL XValidation rules catch most of these
// at admission; this is the wire-level defense in depth for objects that
// slip through (older apiservers without CEL, hand-edited objects, the
// envtest path that bypasses admission for negative-case fixtures).
//
// Accepts: http:// or https://, optional :port (1-65535), optional path
// prefix, IPv6 literal hosts, Punycode hosts.
// Rejects: empty, missing/non-HTTP scheme, opaque URIs, userinfo (@),
// query string (?), fragment (#), whitespace/control chars, empty host,
// out-of-range port, raw Unicode hostnames.
func ValidateEndpoint(raw string) error {
	if strings.TrimSpace(raw) == "" || raw == "/" {
		return errors.New("empty endpoint")
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("whitespace or control character in endpoint")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Opaque != "" {
		return errors.New("opaque endpoint (use scheme://host[:port][/path])")
	}
	if u.User != nil {
		return errors.New("userinfo not allowed (use spec.masterKeySecretRef)")
	}
	if u.RawQuery != "" {
		return errors.New("query string not allowed")
	}
	if u.Fragment != "" {
		return errors.New("fragment not allowed")
	}
	if u.Host == "" {
		return errors.New("host required")
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
	// Reject raw Unicode hostnames; Punycode (xn--...) required. IPv6
	// literals are not strict-DNS labels, so skip the host check for
	// bracketed hosts. idna.Lookup.ToASCII validates label structure
	// (length, hyphen placement, valid Punycode) for ASCII hosts.
	if !strings.HasPrefix(u.Host, "[") {
		for _, r := range host {
			if r > unicode.MaxASCII {
				return errors.New("non-ASCII host (use Punycode xn--... encoding)")
			}
		}
		if _, ierr := idna.Lookup.ToASCII(host); ierr != nil {
			return fmt.Errorf("invalid host label: %w", ierr)
		}
	}
	return nil
}

// ClassifyEndpointTransport reports whether sending the master key to raw
// would traverse plaintext HTTP to a host OUTSIDE the cluster/loopback —
// i.e. a remote that could observe the Bearer token in cleartext (M-SEC2).
//
// It does NOT change ValidateEndpoint's accept set: in-cluster http://*.svc
// and http://localhost remain valid (the common LiteLLM-in-cluster
// deployment). The boolean is advisory; callers decide whether to warn or
// (under LITELLM_OPERATOR_REQUIRE_HTTPS_REMOTE) hard-reject.
//
// insecureRemote == (scheme=="http" && host is neither loopback nor
// cluster-local). https:// is always secure; http:// to loopback or a
// cluster-local name is acceptable.
func ClassifyEndpointTransport(raw string) (insecureRemote bool, err error) {
	if verr := ValidateEndpoint(raw); verr != nil {
		return false, verr
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return false, fmt.Errorf("parse endpoint: %w", perr)
	}
	if u.Scheme != "http" {
		return false, nil // https — secure regardless of host
	}
	host := u.Hostname()
	if isLoopbackHost(host) || isClusterLocalHost(host) {
		return false, nil
	}
	return true, nil
}

// isLoopbackHost matches "localhost" and loopback IP literals.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isClusterLocalHost matches Kubernetes in-cluster service DNS: a bare
// single-label service name (no dot), or a name ending in ".svc" /
// ".svc.cluster.local". IP literals are never cluster-local here.
func isClusterLocalHost(host string) bool {
	if net.ParseIP(host) != nil {
		return false
	}
	if !strings.Contains(host, ".") {
		return true // bare service name, e.g. "litellm"
	}
	h := strings.TrimSuffix(host, ".")
	return strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local")
}
