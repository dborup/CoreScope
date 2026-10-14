package pprofbind

import "testing"

// env builds a getenv func over a literal map. A missing key reads as "",
// which is what os.Getenv does for an unset variable.
func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// Issue #441 (owner decision on #431): with ENABLE_PPROF=true and nothing
// else, each listener binds loopback on its own port. /debug/pprof/ is
// unauthenticated and can stall a process, so reaching it from elsewhere
// is something an operator asks for (PPROF_HOST here, PPROF_BIND for a
// published container port) or gets through an SSH tunnel.
func TestDefaultsBindLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		role     Role
		wantAddr string
	}{
		{Server, "127.0.0.1:6060"},
		{Ingestor, "127.0.0.1:6061"},
	} {
		got := tc.role.Resolve(env(map[string]string{"ENABLE_PPROF": "true"}))
		if !got.Enabled {
			t.Fatalf("%s: want enabled, got %+v", tc.role.Name, got)
		}
		if got.Addr != tc.wantAddr {
			t.Errorf("%s: Addr = %q, want %q (loopback by default since #441)", tc.role.Name, got.Addr, tc.wantAddr)
		}
		if got.Host != "127.0.0.1" {
			t.Errorf("%s: Host = %q, want %q", tc.role.Name, got.Host, "127.0.0.1")
		}
		if got.URL != "http://127.0.0.1:"+got.Port+"/debug/pprof/" {
			t.Errorf("%s: URL = %q", tc.role.Name, got.URL)
		}
	}
}

// The way back to the pre-#441 listener: an explicit wildcard host, shared
// or per role.
func TestWildcardHostRestoresEveryInterface(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   map[string]string
		role Role
		want string
	}{
		{"0.0.0.0", map[string]string{"PPROF_HOST": "0.0.0.0"}, Server, "0.0.0.0:6060"},
		{"star", map[string]string{"PPROF_HOST": "*"}, Ingestor, ":6061"},
		{"ipv6 wildcard", map[string]string{"PPROF_HOST": "::"}, Server, "[::]:6060"},
		{"per role", map[string]string{"PPROF_HOST_INGESTOR": "0.0.0.0"}, Ingestor, "0.0.0.0:6061"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := map[string]string{"ENABLE_PPROF": "true"}
			for k, v := range tc.kv {
				kv[k] = v
			}
			got := tc.role.Resolve(env(kv))
			if !got.Enabled || got.Addr != tc.want {
				t.Errorf("want enabled on %q, got %+v", tc.want, got)
			}
		})
	}
	// A per-role override leaves the other role on the loopback default.
	other := Server.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST_INGESTOR": "0.0.0.0"}))
	if other.Addr != "127.0.0.1:6060" {
		t.Errorf("PPROF_HOST_INGESTOR must not move the server listener; got %q", other.Addr)
	}
}

func TestServerAndIngestorDefaultPortsStayDistinct(t *testing.T) {
	if Server.DefaultPort == Ingestor.DefaultPort {
		t.Fatalf("server and ingestor must keep distinct default pprof ports, both are %q", Server.DefaultPort)
	}
	if Server.DefaultPort != "6060" || Ingestor.DefaultPort != "6061" {
		t.Fatalf("default ports moved: server=%q ingestor=%q, want 6060/6061", Server.DefaultPort, Ingestor.DefaultPort)
	}
	if Server.EnvSuffix == Ingestor.EnvSuffix {
		t.Fatalf("roles must use distinct env suffixes, both are %q", Server.EnvSuffix)
	}
}

func TestHostOptInBindsLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		want string
	}{
		{"ipv4 loopback", "127.0.0.1", "127.0.0.1:6060"},
		{"ipv6 loopback is bracketed", "::1", "[::1]:6060"},
		{"name", "localhost", "localhost:6060"},
		{"padded value is trimmed", "  127.0.0.1  ", "127.0.0.1:6060"},
		{"explicit wildcard means every interface", "0.0.0.0", "0.0.0.0:6060"},
		{"star means every interface", "*", ":6060"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Server.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST": tc.host}))
			if !got.Enabled {
				t.Fatalf("want enabled, got %+v", got)
			}
			if got.Addr != tc.want {
				t.Errorf("Addr = %q, want %q", got.Addr, tc.want)
			}
		})
	}
}

func TestPerRoleHostOverridesSharedHost(t *testing.T) {
	kv := map[string]string{
		"ENABLE_PPROF":        "true",
		"PPROF_HOST":          "127.0.0.1",
		"PPROF_HOST_INGESTOR": "0.0.0.0",
	}
	if got := Server.Resolve(env(kv)); got.Addr != "127.0.0.1:6060" {
		t.Errorf("server Addr = %q, want 127.0.0.1:6060 (shared PPROF_HOST applies)", got.Addr)
	}
	if got := Ingestor.Resolve(env(kv)); got.Addr != "0.0.0.0:6061" {
		t.Errorf("ingestor Addr = %q, want 0.0.0.0:6061 (per-role host wins over shared)", got.Addr)
	}
}

func TestSharedPortStillAppliesToBothRoles(t *testing.T) {
	// PPROF_PORT predates #372 and applied to whichever process read it.
	// Keep that so an existing install that sets it is unaffected.
	kv := map[string]string{"ENABLE_PPROF": "true", "PPROF_PORT": "7000"}
	if got := Server.Resolve(env(kv)); got.Addr != "127.0.0.1:7000" {
		t.Errorf("server Addr = %q, want 127.0.0.1:7000", got.Addr)
	}
	if got := Ingestor.Resolve(env(kv)); got.Addr != "127.0.0.1:7000" {
		t.Errorf("ingestor Addr = %q, want 127.0.0.1:7000", got.Addr)
	}
}

func TestPerRolePortOverridesSharedPort(t *testing.T) {
	kv := map[string]string{
		"ENABLE_PPROF":      "true",
		"PPROF_PORT":        "7000",
		"PPROF_PORT_SERVER": "7100",
	}
	if got := Server.Resolve(env(kv)); got.Addr != "127.0.0.1:7100" {
		t.Errorf("server Addr = %q, want 127.0.0.1:7100 (per-role port wins)", got.Addr)
	}
	if got := Ingestor.Resolve(env(kv)); got.Addr != "127.0.0.1:7000" {
		t.Errorf("ingestor Addr = %q, want 127.0.0.1:7000 (shared port still applies)", got.Addr)
	}
}

func TestDisabledUnlessEnablePprofIsExactlyTrue(t *testing.T) {
	for _, tc := range []struct {
		value      string
		wantReason bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"true", false}, // enabled; checked below
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
	} {
		got := Server.Resolve(env(map[string]string{"ENABLE_PPROF": tc.value}))
		if tc.value == "true" {
			if !got.Enabled {
				t.Errorf("ENABLE_PPROF=%q: want enabled", tc.value)
			}
			continue
		}
		if got.Enabled {
			t.Errorf("ENABLE_PPROF=%q: want pprof absent, got %+v", tc.value, got)
		}
		if got.Addr != "" {
			t.Errorf("ENABLE_PPROF=%q: Addr = %q, want empty when disabled", tc.value, got.Addr)
		}
		if tc.wantReason && got.Reason == "" {
			t.Errorf("ENABLE_PPROF=%q: a truthy-looking value that is not exactly %q must report why pprof stayed off", tc.value, "true")
		}
		if !tc.wantReason && got.Reason != "" {
			t.Errorf("ENABLE_PPROF=%q: want no reason for an unambiguous off value, got %q", tc.value, got.Reason)
		}
	}
}

func TestUnusablePortDisablesWithAReason(t *testing.T) {
	for _, port := range []string{"abc", "0", "-1", "65536", "60 60", "6060/tcp", ""} {
		kv := map[string]string{"ENABLE_PPROF": "true", "PPROF_PORT_SERVER": port}
		got := Server.Resolve(env(kv))
		if port == "" {
			// Empty reads as unset: fall through to the default.
			if !got.Enabled || got.Addr != "127.0.0.1:6060" {
				t.Errorf("PPROF_PORT_SERVER=%q: want the default :6060, got %+v", port, got)
			}
			continue
		}
		if got.Enabled {
			t.Errorf("PPROF_PORT_SERVER=%q: want disabled, got %+v", port, got)
		}
		if got.Reason == "" {
			t.Errorf("PPROF_PORT_SERVER=%q: want a reason", port)
		}
	}
}

func TestUnusableHostDisablesWithAReason(t *testing.T) {
	for _, host := range []string{"127.0.0.1/8", "127.0.0.1 ", "local host", "host:1234"} {
		got := Server.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST": host}))
		if host == "127.0.0.1 " {
			if !got.Enabled || got.Addr != "127.0.0.1:6060" {
				t.Errorf("PPROF_HOST=%q: trailing space must be trimmed, got %+v", host, got)
			}
			continue
		}
		if got.Enabled {
			t.Errorf("PPROF_HOST=%q: want disabled, got %+v", host, got)
		}
		if got.Reason == "" {
			t.Errorf("PPROF_HOST=%q: want a reason", host)
		}
	}
}

func TestURLNamesLocalhostWhenBoundEverywhere(t *testing.T) {
	got := Server.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST": "*"}))
	if got.URL != "http://localhost:6060/debug/pprof/" {
		t.Errorf("URL = %q, want http://localhost:6060/debug/pprof/", got.URL)
	}
	// An unspecified address is every interface too (the staging compose
	// files pin PPROF_HOST=0.0.0.0), so it names loopback the same way.
	for _, host := range []string{"0.0.0.0", "::"} {
		got = Server.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST": host}))
		if got.URL != "http://localhost:6060/debug/pprof/" {
			t.Errorf("PPROF_HOST=%s: URL = %q, want http://localhost:6060/debug/pprof/", host, got.URL)
		}
	}
	got = Ingestor.Resolve(env(map[string]string{"ENABLE_PPROF": "true", "PPROF_HOST": "::1"}))
	if got.URL != "http://[::1]:6061/debug/pprof/" {
		t.Errorf("URL = %q, want http://[::1]:6061/debug/pprof/", got.URL)
	}
}

// TestEmptyEnvironmentIsInert pins the other half of review round 2,
// finding 3. The compose files forward every pprof setting with an empty
// default, so an install that never set one now has PPROF_HOST="",
// PPROF_PORT="" and so on PRESENT in the container's environment rather
// than absent. Present-but-empty must behave exactly like absent, or
// forwarding them would itself be the behaviour change.
func TestEmptyEnvironmentIsInert(t *testing.T) {
	empty := map[string]string{
		"ENABLE_PPROF":        "",
		"PPROF_HOST":          "",
		"PPROF_HOST_SERVER":   "",
		"PPROF_HOST_INGESTOR": "",
		"PPROF_PORT":          "",
		"PPROF_PORT_SERVER":   "",
		"PPROF_PORT_INGESTOR": "",
	}
	for _, role := range []Role{Server, Ingestor} {
		got := role.Resolve(env(empty))
		if got.Enabled {
			t.Errorf("%s: an all-empty environment must leave pprof off; got %+v", role.Name, got)
		}
		if got.Reason != "" {
			t.Errorf("%s: an all-empty environment must say nothing; got reason %q", role.Name, got.Reason)
		}
	}
	// With pprof asked for, the empty host and port settings must resolve
	// to the default listener: loopback (#441), the role's own port.
	for _, tc := range []struct {
		role     Role
		wantAddr string
	}{
		{Server, "127.0.0.1:6060"},
		{Ingestor, "127.0.0.1:6061"},
	} {
		on := map[string]string{"ENABLE_PPROF": "true"}
		for k, v := range empty {
			if k == "ENABLE_PPROF" {
				continue
			}
			on[k] = v
		}
		got := tc.role.Resolve(env(on))
		if !got.Enabled || got.Addr != tc.wantAddr {
			t.Errorf("%s: empty host/port settings must resolve to %q; got %+v", tc.role.Name, tc.wantAddr, got)
		}
	}
}
