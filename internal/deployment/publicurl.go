// Package deployment validates the deployment's own public addresses.
// External MIREA and internal Docker addresses are separate dependencies.
package deployment

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const SiteURL = "https://aa.ytc-srv.org"
const IdentityURL = "https://aaid.ytc-srv.org"

func PublicURL(raw, fallback string) (string, error) {
	if raw == "" {
		raw = fallback
	}
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	invalid := errors.New("public URL must be an HTTP(S) origin under *.ytc-srv.org or a local loopback origin")
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", invalid
		}
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && !strings.HasSuffix(host, ".ytc-srv.org") {
		return "", invalid
	}
	return strings.TrimSuffix(raw, "/"), nil
}
