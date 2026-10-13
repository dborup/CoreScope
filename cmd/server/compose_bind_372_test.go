package main

// Behavioural guard tests for issue #372: the bundled MQTT broker's host
// port and the pprof host ports must have an EXPLICIT, configurable bind
// interface, and the shipped default of each must keep today's exposure so
// an existing install that pulls the new compose file changes nothing.
//
// The shape is asserted with regex over comment-stripped YAML, reusing the
// helpers in staging_compose_broker_test.go, for the reasons documented
// there (no yaml dependency in cmd/server, cosmetic edits must not break
// the tests, a `# 1883:1883` example in prose must not pass as a binding).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// portsBlockOf returns the comment-stripped ports: sub-block of one service.
func portsBlockOf(t *testing.T, compose, service string) string {
	t.Helper()
	yaml := readRepoCompose(t, compose)
	block := extractServiceBlock(stripYAMLComments(yaml), service)
	if strings.TrimSpace(block) == "" {
		t.Fatalf("%s: no services.%s block", compose, service)
	}
	ports := extractSubBlock(block, "ports", 4)
	if strings.TrimSpace(ports) == "" {
		t.Fatalf("%s: services.%s has no ports: sub-block", compose, service)
	}
	return ports
}

// bindMapping matches a published port whose host interface comes from an
// interpolated variable, e.g.
//
//   - "${PROD_MQTT_BIND:-0.0.0.0}:${PROD_MQTT_PORT:-1883}:1883"
//
// capturing the variable name and its default.
func bindMapping(variable, hostPort, containerPort string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*-\s*"\$\{` + regexp.QuoteMeta(variable) + `:-([^}]*)\}:` + hostPort + `:` + containerPort + `"\s*$`)
}

func TestProdCompose_BundledMQTTHostPortBindIsExplicit(t *testing.T) {
	ports := portsBlockOf(t, "docker-compose.yml", "prod")

	re := bindMapping("PROD_MQTT_BIND", `\$\{PROD_MQTT_PORT:-1883\}`, "1883")
	m := re.FindStringSubmatch(ports)
	if m == nil {
		t.Fatalf("docker-compose.yml: the bundled broker's host port must publish through an explicit bind interface, i.e.\n"+
			"      - \"${PROD_MQTT_BIND:-0.0.0.0}:${PROD_MQTT_PORT:-1883}:1883\"\nports block:\n%s", ports)
	}
	// The default must keep today's exposure. Narrowing it to loopback is a
	// deployment-visible flip and is explicitly left to the owner, not made
	// here: an install that publishes 1883 for its gateways would lose them
	// the moment it pulled this file.
	if m[1] != "0.0.0.0" {
		t.Fatalf("PROD_MQTT_BIND default is %q, want \"0.0.0.0\" — the shipped default must not change an existing install's MQTT exposure", m[1])
	}
}

func TestProdCompose_NoUnboundMQTTMappingRemains(t *testing.T) {
	ports := portsBlockOf(t, "docker-compose.yml", "prod")
	// A two-field mapping ("1883:1883", "${PROD_MQTT_PORT:-1883}:1883")
	// publishes on every interface with no way to narrow it. Only the
	// three-field form with a bind variable is allowed for 1883.
	unbound := regexp.MustCompile(`(?m)^\s*-\s*"?[^":\s]*(?::[^":\s]*)?:1883"?\s*$`)
	for _, ln := range strings.Split(ports, "\n") {
		if !unbound.MatchString(ln) {
			continue
		}
		if bindMapping("PROD_MQTT_BIND", `\$\{PROD_MQTT_PORT:-1883\}`, "1883").MatchString(ln) {
			continue
		}
		t.Fatalf("docker-compose.yml publishes 1883 without a configurable bind interface: %q", strings.TrimSpace(ln))
	}
}

func TestStagingComposes_PprofHostPortsBindExplicitlyAndStayDistinct(t *testing.T) {
	for _, compose := range []string{
		"docker-compose.staging.yml",
		"docker-compose.staging.no-mosquitto.yml",
	} {
		t.Run(compose, func(t *testing.T) {
			ports := portsBlockOf(t, compose, "staging-go")
			seen := map[string]bool{}
			for _, port := range []string{"6060", "6061"} {
				re := bindMapping("PPROF_BIND", port, port)
				m := re.FindStringSubmatch(ports)
				if m == nil {
					t.Fatalf("%s: pprof port %s must publish through an explicit bind interface, i.e.\n"+
						"      - \"${PPROF_BIND:-0.0.0.0}:%s:%s\"\nports block:\n%s", compose, port, port, port, ports)
				}
				// meshview-pve profiles this host remotely; narrowing the
				// default to loopback would cut that off on the next pull.
				if m[1] != "0.0.0.0" {
					t.Fatalf("%s: PPROF_BIND default for %s is %q, want \"0.0.0.0\"", compose, port, m[1])
				}
				if seen[port] {
					t.Fatalf("%s: port %s published twice", compose, port)
				}
				seen[port] = true
			}
			// The server and ingestor listeners must not be collapsed onto
			// one port: a single pprof endpoint would silently profile only
			// whichever process won the bind race.
			if len(seen) != 2 {
				t.Fatalf("%s: want distinct pprof ports 6060 and 6061, got %v", compose, seen)
			}
			bare := regexp.MustCompile(`(?m)^\s*-\s*"?(606[01]):606[01]"?\s*$`)
			if m := bare.FindString(ports); m != "" {
				t.Fatalf("%s: pprof mapping %q has no configurable bind interface", compose, strings.TrimSpace(m))
			}
		})
	}
}

func TestEnvExample_DocumentsEveryNewKnob(t *testing.T) {
	env := readRepoFile(t, ".env.example")
	// Each knob added by #372 must be discoverable in the one file both
	// docker compose and manage.sh read, with its default spelled out —
	// otherwise an operator cannot find the setting that keeps or changes
	// their exposure.
	for _, knob := range []struct{ key, wantDefault string }{
		{"PROD_MQTT_BIND", "0.0.0.0"},
		{"PPROF_BIND", "0.0.0.0"},
		{"PPROF_HOST", ""},
		{"PPROF_PORT_SERVER", "6060"},
		{"PPROF_PORT_INGESTOR", "6061"},
		{"MQTT_SOURCE_STRICT", "false"},
	} {
		assign := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(knob.key) + `=`)
		if !assign.MatchString(env) {
			t.Errorf(".env.example must document %s=", knob.key)
			continue
		}
		if knob.wantDefault == "" {
			continue
		}
		doc := regexp.MustCompile(`(?m)^# Default: ` + regexp.QuoteMeta(knob.wantDefault) + `\s*$`)
		section := knobSection(env, knob.key)
		if !doc.MatchString(section) {
			t.Errorf(".env.example must state `# Default: %s` for %s; its section was:\n%s", knob.wantDefault, knob.key, section)
		}
	}
}

// knobSection returns the comment block immediately above a KEY= line, which
// is where .env.example records each key's default and consumers.
func knobSection(env, key string) string {
	lines := strings.Split(env, "\n")
	for i, ln := range lines {
		if !strings.HasPrefix(ln, key+"=") {
			continue
		}
		start := i
		for start > 0 && strings.HasPrefix(lines[start-1], "#") {
			start--
		}
		return strings.Join(lines[start:i+1], "\n")
	}
	return ""
}

func TestDeploymentDocs_CarryTheComposePullMigrationNote(t *testing.T) {
	doc := readRepoFile(t, "docs/deployment.md")
	heading := regexp.MustCompile(`(?m)^#{2,4} .*[Mm]igration.*[Cc]ompose`)
	if !heading.MatchString(doc) {
		t.Fatalf("docs/deployment.md must carry a migration heading for operators pulling the new compose file")
	}
	// The note is only useful if it names every knob an existing install
	// might need to touch, and says plainly that nothing changes by default.
	for _, want := range []string{
		"PROD_MQTT_BIND",
		"PPROF_BIND",
		"PPROF_HOST",
		"MQTT_SOURCE_STRICT",
		"DISABLE_MOSQUITTO",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/deployment.md migration note must name %s", want)
		}
	}
	if !regexp.MustCompile(`(?i)no change|unchanged|nothing changes`).MatchString(doc) {
		t.Errorf("docs/deployment.md must state that the shipped defaults leave an existing install unchanged")
	}
}

func TestNoComposeFileHardcodesALoopbackBind(t *testing.T) {
	// A loopback literal baked into a published port is the non-inert flip
	// this issue explicitly defers to the owner. Catch it in every compose
	// file, not just the ones with named tests above.
	entries, err := filepath.Glob(filepath.Join("..", "..", "docker-compose*.yml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no docker-compose*.yml files found")
	}
	loopback := regexp.MustCompile(`(?m)^\s*-\s*"?(127\.0\.0\.1|\[::1\]|localhost):`)
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if m := loopback.FindString(stripYAMLComments(string(data))); m != "" {
			t.Errorf("%s hardcodes a loopback publish (%s); use the bind variable so operators keep the choice",
				filepath.Base(path), strings.TrimSpace(m))
		}
	}
}
