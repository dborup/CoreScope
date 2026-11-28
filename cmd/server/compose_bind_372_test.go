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

// bindMapping matches a published port whose host interface is CONDITIONALLY
// interpolated, e.g.
//
//   - "${PROD_MQTT_BIND:+${PROD_MQTT_BIND}:}${PROD_MQTT_PORT:-1883}:1883"
//
// The `:+` form is the only one that is inert when the variable is unset.
// A `:-` default, even `0.0.0.0`, is NOT inert: docker publishes an
// unqualified mapping on both 0.0.0.0 and [::], while `0.0.0.0:` publishes
// IPv4 only, so a `:-0.0.0.0` default silently drops IPv6 reachability on
// the next `up -d`. See TestComposeBindDefaultsStayDualStack.
func bindMapping(variable, hostPort, containerPort string) *regexp.Regexp {
	v := regexp.QuoteMeta(variable)
	return regexp.MustCompile(`(?m)^\s*-\s*"\$\{` + v + `:\+\$\{` + v + `\}:\}` + hostPort + `:` + containerPort + `"\s*$`)
}

// anyHostBindDefault matches a published port whose host interface is
// interpolated with a `:-` DEFAULT, capturing the variable and the default.
// Any match is a bug: see TestComposeBindDefaultsStayDualStack.
// The trailing `[^"]*:[^"]*` requires a second colon after the interpolated
// host interface, which is what distinguishes a three-field mapping
// (host:hostPort:containerPort) from an ordinary two-field one such as
// `"${PROD_HTTP_PORT:-80}:80"`.
var anyHostBindDefault = regexp.MustCompile(`(?m)^\s*-\s*"\$\{([A-Za-z_][A-Za-z0-9_]*):-([^}]*)\}:[^"]*:[^"]*"\s*$`)

func TestProdCompose_BundledMQTTHostPortBindIsExplicit(t *testing.T) {
	ports := portsBlockOf(t, "docker-compose.yml", "prod")

	re := bindMapping("PROD_MQTT_BIND", `\$\{PROD_MQTT_PORT:-1883\}`, "1883")
	if !re.MatchString(ports) {
		t.Fatalf("docker-compose.yml: the bundled broker's host port must publish through an explicit, CONDITIONAL bind interface, i.e.\n"+
			"      - \"${PROD_MQTT_BIND:+${PROD_MQTT_BIND}:}${PROD_MQTT_PORT:-1883}:1883\"\nports block:\n%s", ports)
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
				// #441 moved the default from the conditional, dual-stack
				// form to loopback; TestStagingComposes_PprofPublishDefaultsToLoopback
				// pins the exact shape.
				re := pprofLoopbackMapping(port)
				if !re.MatchString(ports) {
					t.Fatalf("%s: pprof port %s must publish through the PPROF_BIND bind interface, i.e.\n"+
						"      - \"${PPROF_BIND:-127.0.0.1}:%s:%s\"\nports block:\n%s", compose, port, port, port, ports)
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
		// PROD_MQTT_BIND defaults to EMPTY, not 0.0.0.0: an unqualified
		// docker mapping is dual-stack and 0.0.0.0: is not, so an explicit
		// 0.0.0.0 default would silently drop IPv6. The pprof knobs default
		// to loopback since #441.
		{"PROD_MQTT_BIND", "(empty - every interface, IPv4 and IPv6)"},
		{"PPROF_BIND", "127.0.0.1"},
		{"PPROF_HOST", "127.0.0.1"},
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
	full := readRepoFile(t, "docs/deployment.md")
	heading := regexp.MustCompile(`(?m)^#{2,4} .*[Mm]igration.*[Cc]ompose`)
	loc := heading.FindStringIndex(full)
	if loc == nil {
		t.Fatalf("docs/deployment.md must carry a migration heading for operators pulling the new compose file")
	}
	// Scope the assertions to the migration section itself. Searching the
	// whole file would pass on a mention anywhere else in the document,
	// which is not where an operator pulling the new file is reading.
	doc := full[loc[0]:]
	if next := regexp.MustCompile(`(?m)^## `).FindStringIndex(doc[3:]); next != nil {
		doc = doc[:next[0]+3]
	}
	// The note is only useful if it names every knob an existing install
	// might need to touch, and says plainly that nothing changes by default.
	for _, want := range []string{
		"PROD_MQTT_BIND",
		"PPROF_BIND",
		"PPROF_HOST",
		"MQTT_SOURCE_STRICT",
		"DISABLE_MOSQUITTO",
		// Forwarding MQTT_BROKER into the container (review round 2,
		// finding 3) is inert for an install that never set it, but NOT for
		// one that has it in .env and relied on config.json while the
		// container ignored it. That is the one non-inert case in this
		// release, so the migration note has to name it.
		"MQTT_BROKER",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/deployment.md migration note must name %s", want)
		}
	}
	if !regexp.MustCompile(`(?i)no change|unchanged|nothing changes`).MatchString(doc) {
		t.Errorf("docs/deployment.md must state that the shipped defaults leave an existing install unchanged")
	}
	// Review round 2, finding 3 has one non-inert consequence: forwarding
	// MQTT_BROKER means a host that has it in .env — where the container
	// used to ignore it — now has it OVERRIDE config.json's mqttSources.
	// That is the only behaviour an existing install can see change, so the
	// migration note must call it out rather than claim a blanket "nothing
	// changes".
	if !regexp.MustCompile(`(?s)MQTT_BROKER.{0,600}?(?i:override|takes precedence|replaces|wins)`).MatchString(doc) &&
		!regexp.MustCompile(`(?s)(?i:override|takes precedence|replaces|wins).{0,600}?MQTT_BROKER`).MatchString(doc) {
		t.Errorf("the migration note must warn that a MQTT_BROKER already in .env now reaches the ingestor and overrides config.json; section was:\n%s", doc)
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

// TestComposeBindDefaultsStayDualStack is the regression guard for the only
// way this issue's "explicit bind" change could alter an existing install:
// the DEFAULT of a publish-bind variable.
//
// Docker publishes a two-field mapping ("1883:1883") on BOTH 0.0.0.0 and
// [::], but a three-field mapping with an IPv4 host ("0.0.0.0:1883:1883")
// publishes IPv4 only. So `${X:-0.0.0.0}:` is NOT equivalent to the old
// unqualified mapping — it drops IPv6 reachability on the next `up -d`.
// Only the conditional `${X:+${X}:}` form, which renders to nothing when X
// is unset, keeps the dual-stack publish.
func TestComposeBindDefaultsStayDualStack(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("..", "..", "docker-compose*.yml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no docker-compose*.yml files found")
	}
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range anyHostBindDefault.FindAllStringSubmatch(stripYAMLComments(string(data)), -1) {
			// The one owner-approved default (#441): the pprof ports are
			// published on loopback unless PPROF_BIND widens them. It
			// narrows rather than drops part of the old exposure, and is
			// pinned by TestStagingComposes_PprofPublishDefaultsToLoopback.
			if m[1] == "PPROF_BIND" && m[2] == "127.0.0.1" {
				continue
			}
			t.Errorf("%s publishes a port whose host interface defaults to %q via ${%s:-...}; "+
				"a defaulted host interface is never equivalent to the old unqualified mapping "+
				"(0.0.0.0: is IPv4-only, the unqualified form is dual-stack). Use ${%s:+${%s}:} instead.",
				filepath.Base(path), m[2], m[1], m[1], m[1])
		}
	}
}

// appComposeServices are the compose files that run the CoreScope image, and
// the service that does so in each. Every in-process setting .env.example
// labels "Used by: the server" or "Used by: the ingestor" has to reach each
// of them, or the documented one-line .env change does nothing.
var appComposeServices = []struct{ file, service string }{
	{"docker-compose.yml", "prod"},
	{"docker-compose.no-mosquitto.yml", "prod"},
	{"docker-compose.staging.yml", "staging-go"},
	{"docker-compose.staging.no-mosquitto.yml", "staging-go"},
	{"docker-compose.example.yml", "corescope"},
}

// processKnobsFromEnvExample returns the keys .env.example says a CoreScope
// PROCESS reads (as opposed to the ones docker compose or manage.sh read
// themselves). .env.example is the source of truth so a knob added there
// cannot be forgotten here.
func processKnobsFromEnvExample(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(readRepoFile(t, ".env.example"), "\n")
	usedBy := regexp.MustCompile(`^# Used by: .*\b(server|ingestor)\b`)
	var keys []string
	for i, ln := range lines {
		if !usedBy.MatchString(ln) {
			continue
		}
		// The KEY= line is the first non-comment line after the block.
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "#") {
				continue
			}
			if k, _, ok := strings.Cut(lines[j], "="); ok && k != "" {
				keys = append(keys, k)
			}
			break
		}
	}
	if len(keys) == 0 {
		t.Fatal(".env.example documents no process-read knobs; the parser is broken")
	}
	return keys
}

// TestComposeFilesPassEveryProcessKnobToTheContainer is review round 2,
// finding 3: MQTT_SOURCE_STRICT, MQTT_BROKER and the PPROF_* settings were
// documented as one-line .env changes, but the compose files passed only
// NODE_ENV, DISABLE_MOSQUITTO and DISABLE_CADDY and declared no env_file, so
// those settings never reached the process that reads them. manage.sh's
// broker preflight then honoured MQTT_BROKER from .env and reported PASS
// while the container ignored it and fell back to mqtt://localhost:1883.
func TestComposeFilesPassEveryProcessKnobToTheContainer(t *testing.T) {
	knobs := processKnobsFromEnvExample(t)
	for _, target := range appComposeServices {
		t.Run(target.file, func(t *testing.T) {
			yaml := stripYAMLComments(readRepoCompose(t, target.file))
			block := extractServiceBlock(yaml, target.service)
			if strings.TrimSpace(block) == "" {
				t.Fatalf("%s: no services.%s block", target.file, target.service)
			}
			if strings.Contains(block, "env_file") {
				return // the whole .env reaches the container
			}
			env := extractSubBlock(block, "environment", 4)
			if strings.TrimSpace(env) == "" {
				t.Fatalf("%s: services.%s has no environment: sub-block", target.file, target.service)
			}
			for _, knob := range knobs {
				assigned := regexp.MustCompile(`(?m)^\s*-\s*` + regexp.QuoteMeta(knob) + `=(\S*)\s*$`)
				m := assigned.FindStringSubmatch(env)
				if m == nil {
					t.Errorf("%s: services.%s must pass %s through to the container, or an operator who sets it in .env changes nothing\nenvironment block:\n%s",
						target.file, target.service, knob, env)
					continue
				}
				// A hard-coded value is this file's deliberate choice
				// (ENABLE_PPROF=true on staging); an interpolated one must
				// default to EMPTY so an unset knob stays inert.
				if !strings.HasPrefix(m[1], "${") {
					continue
				}
				if want := "${" + knob + ":-}"; m[1] != want {
					t.Errorf("%s: services.%s passes %s as %s, want %s — any other default changes behaviour for an install that never set it",
						target.file, target.service, knob, m[1], want)
				}
			}
		})
	}
}
