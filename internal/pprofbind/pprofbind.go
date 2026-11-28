// Package pprofbind resolves where a CoreScope process exposes
// net/http/pprof, from the environment alone, for one process role.
//
// pprof is opt-in: it stays absent unless ENABLE_PPROF is exactly "true",
// the spelling docker/entrypoint-go.sh and the compose files already use. A
// truthy-looking value that is not that exact spelling leaves pprof off and
// reports why, so an operator who typed ENABLE_PPROF=1 is told rather than
// left wondering.
//
// The bind host is a separate, explicit setting. Its default is loopback,
// 127.0.0.1 (#441): /debug/pprof/ is unauthenticated and can stall a
// process, so it is reached through an SSH tunnel unless an operator widens
// it with PPROF_HOST (or the per-role PPROF_HOST_<ROLE>) — "0.0.0.0" or "*"
// for every interface, the pre-#441 listener. A container whose compose
// file publishes the pprof ports pins PPROF_HOST to every interface itself
// and narrows the published port with PPROF_BIND, because docker's port
// proxy cannot reach a loopback listener inside the container.
//
// Ports stay distinct per role: the server defaults to 6060 and the
// ingestor to 6061. The shared PPROF_PORT predates this package and still
// applies to both processes, which is why PPROF_PORT_<ROLE> exists — it is
// the only way to move one listener without colliding the other.
//
// Precedence, highest first:
//
//	PPROF_HOST_<ROLE>  PPROF_PORT_<ROLE>
//	PPROF_HOST         PPROF_PORT
//	DefaultHost        Role.DefaultPort
package pprofbind

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Role is one CoreScope process that can expose pprof. Its EnvSuffix names
// the per-role environment variables (PPROF_HOST_SERVER, PPROF_PORT_SERVER)
// and its DefaultPort is the port it uses when nothing in the environment
// picks one.
type Role struct {
	Name        string
	EnvSuffix   string
	DefaultPort string
}

// DefaultHost is where a listener binds when no PPROF_HOST setting names a
// host.
const DefaultHost = "127.0.0.1"

// Server and Ingestor are the two roles. Their default ports differ so both
// listeners can run side by side in one container, which is how
// docker-compose.staging.yml publishes them.
var (
	Server   = Role{Name: "server", EnvSuffix: "SERVER", DefaultPort: "6060"}
	Ingestor = Role{Name: "ingestor", EnvSuffix: "INGESTOR", DefaultPort: "6061"}
)

// Config is a resolved pprof listener. When Enabled is false the listener
// must not start; Reason then holds an operator-facing explanation when the
// environment looked like it was asking for pprof, and is empty when pprof
// was simply not requested.
type Config struct {
	Enabled bool
	Addr    string // for http.ListenAndServe; empty when disabled
	Host    string // resolved bind host; empty means every interface
	Port    string
	URL     string // human-readable profiling URL
	Reason  string
}

// Resolve reads the environment through getenv (os.Getenv in production,
// a map in tests) and returns the listener for this role.
func (r Role) Resolve(getenv func(string) string) Config {
	switch enable := getenv("ENABLE_PPROF"); enable {
	case "true":
	case "", "false", "0", "no", "off":
		return Config{}
	default:
		return Config{Reason: fmt.Sprintf("ENABLE_PPROF=%q is not %q, so %s pprof stays off; set it to the exact value %q to enable profiling", enable, "true", r.Name, "true")}
	}

	port, err := r.resolvePort(getenv)
	if err != nil {
		return Config{Reason: fmt.Sprintf("%s pprof stays off: %v", r.Name, err)}
	}
	host, err := r.resolveHost(getenv)
	if err != nil {
		return Config{Reason: fmt.Sprintf("%s pprof stays off: %v", r.Name, err)}
	}

	addr := ":" + port
	if host != "" {
		addr = net.JoinHostPort(host, port)
	}
	// A listener on every interface ("" or an unspecified address such as
	// 0.0.0.0) has no single hostname to print, so the URL names loopback —
	// the one address that always reaches it.
	display := host
	if ip := net.ParseIP(host); display == "" || (ip != nil && ip.IsUnspecified()) {
		display = "localhost"
	}
	return Config{
		Enabled: true,
		Addr:    addr,
		Host:    host,
		Port:    port,
		URL:     "http://" + net.JoinHostPort(display, port) + "/debug/pprof/",
	}
}

// resolvePort applies the port precedence and rejects anything that is not
// a bindable TCP port number. An unusable value disables pprof rather than
// silently falling back to the default port: the default may already be
// taken by the other role's listener.
func (r Role) resolvePort(getenv func(string) string) (string, error) {
	raw, from := r.DefaultPort, "default"
	if v := strings.TrimSpace(getenv("PPROF_PORT")); v != "" {
		raw, from = v, "PPROF_PORT"
	}
	if v := strings.TrimSpace(getenv("PPROF_PORT_" + r.EnvSuffix)); v != "" {
		raw, from = v, "PPROF_PORT_"+r.EnvSuffix
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%s=%q is not a TCP port between 1 and 65535", from, raw)
	}
	return strconv.Itoa(n), nil
}

// resolveHost applies the host precedence. "*" means every interface, as
// does an unspecified address such as "0.0.0.0". Anything that is not a
// bare host — a CIDR mask, a host:port pair, embedded whitespace — is
// rejected rather than guessed at.
func (r Role) resolveHost(getenv func(string) string) (string, error) {
	raw, from := DefaultHost, "default"
	if v := strings.TrimSpace(getenv("PPROF_HOST")); v != "" {
		raw, from = v, "PPROF_HOST"
	}
	if v := strings.TrimSpace(getenv("PPROF_HOST_" + r.EnvSuffix)); v != "" {
		raw, from = v, "PPROF_HOST_"+r.EnvSuffix
	}
	if raw == "*" {
		return "", nil
	}
	if strings.ContainsAny(raw, " \t/?#@") {
		return "", fmt.Errorf("%s=%q is not a bare host name or IP address", from, raw)
	}
	// A bracketless IPv6 address is fine (net.JoinHostPort brackets it);
	// a host:port pair is not, so reject a lone colon that is not part of
	// an IP address.
	if strings.Contains(raw, ":") && net.ParseIP(raw) == nil {
		return "", fmt.Errorf("%s=%q must be a host only — the port comes from PPROF_PORT_%s", from, raw, r.EnvSuffix)
	}
	return raw, nil
}
