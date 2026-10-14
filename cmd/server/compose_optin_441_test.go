package main

// Shape tests for issue #441, the owner decisions on #431:
//
//  1. The bundled mosquitto is opt-in: every compose file renders
//     DISABLE_MOSQUITTO=true when the variable is unset.
//  3. pprof is loopback by default when enabled: the staging files publish
//     6060/6061 on 127.0.0.1 unless PPROF_BIND says otherwise, the
//     in-container listener stays reachable by that publish, and the
//     files that publish nothing leave the listener on its own loopback
//     default.
//
// The helpers (readRepoCompose, stripYAMLComments, extractServiceBlock,
// extractSubBlock, portsBlockOf, readRepoFile, appComposeServices) live in
// staging_compose_broker_test.go and compose_bind_372_test.go.

import (
	"regexp"
	"strings"
	"testing"
)

func envBlockOf(t *testing.T, compose, service string) string {
	t.Helper()
	block := extractServiceBlock(stripYAMLComments(readRepoCompose(t, compose)), service)
	if strings.TrimSpace(block) == "" {
		t.Fatalf("%s: no services.%s block", compose, service)
	}
	env := extractSubBlock(block, "environment", 4)
	if strings.TrimSpace(env) == "" {
		t.Fatalf("%s: services.%s has no environment: sub-block", compose, service)
	}
	return env
}

func TestComposeFiles_BundledBrokerIsOptIn(t *testing.T) {
	assign := regexp.MustCompile(`(?m)^\s*-\s*DISABLE_MOSQUITTO=(\S*)\s*$`)
	for _, target := range appComposeServices {
		t.Run(target.file, func(t *testing.T) {
			env := envBlockOf(t, target.file, target.service)
			all := assign.FindAllStringSubmatch(env, -1)
			if len(all) != 1 {
				t.Fatalf("want exactly one DISABLE_MOSQUITTO assignment, got %d\nenvironment block:\n%s", len(all), env)
			}
			switch got := all[0][1]; got {
			case "${DISABLE_MOSQUITTO:-true}":
			case "true":
				// The no-mosquitto files exist to run without the bundled
				// broker; they may pin it.
				if !strings.Contains(target.file, "no-mosquitto") {
					t.Errorf("%s pins DISABLE_MOSQUITTO=true, leaving no way to opt in to the bundled broker; use ${DISABLE_MOSQUITTO:-true}", target.file)
				}
			default:
				t.Errorf("%s passes DISABLE_MOSQUITTO=%s; the bundled broker is opt-in, so an unset value must render true (${DISABLE_MOSQUITTO:-true})", target.file, got)
			}
		})
	}
}

// pprofLoopbackMapping matches a pprof port published on PPROF_BIND with a
// 127.0.0.1 default, e.g. "${PPROF_BIND:-127.0.0.1}:6060:6060".
func pprofLoopbackMapping(port string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*-\s*"\$\{PPROF_BIND:-127\.0\.0\.1\}:` + port + `:` + port + `"\s*$`)
}

// pprofPublishers are the compose files that publish the pprof ports.
var pprofPublishers = []string{
	"docker-compose.staging.yml",
	"docker-compose.staging.no-mosquitto.yml",
}

func TestStagingComposes_PprofPublishDefaultsToLoopback(t *testing.T) {
	for _, compose := range pprofPublishers {
		t.Run(compose, func(t *testing.T) {
			ports := portsBlockOf(t, compose, "staging-go")
			for _, port := range []string{"6060", "6061"} {
				if !pprofLoopbackMapping(port).MatchString(ports) {
					t.Errorf("%s: pprof port %s must publish on PPROF_BIND, defaulting to loopback:\n"+
						"      - \"${PPROF_BIND:-127.0.0.1}:%s:%s\"\nports block:\n%s", compose, port, port, port, ports)
				}
			}
		})
	}
}

// A loopback listener INSIDE the container is unreachable through a docker
// port mapping: docker's proxy connects to the container's network
// address. Where pprof is published, the in-container listener must
// therefore bind every interface, and PPROF_BIND alone decides who on the
// host can reach it. Without this, the #441 loopback default in pprofbind
// would silently kill the published port even with PPROF_BIND=0.0.0.0.
func TestStagingComposes_InContainerPprofListenerStaysReachable(t *testing.T) {
	wildcard := regexp.MustCompile(`(?m)^\s*-\s*PPROF_HOST=0\.0\.0\.0\s*$`)
	for _, compose := range pprofPublishers {
		t.Run(compose, func(t *testing.T) {
			env := envBlockOf(t, compose, "staging-go")
			if !wildcard.MatchString(env) {
				t.Errorf("%s publishes pprof, so it must pin the in-container listener to every interface (PPROF_HOST=0.0.0.0)\nenvironment block:\n%s", compose, env)
			}
		})
	}
}

// The compose files that publish no pprof port must not widen the
// in-container listener: with nothing published, the loopback default is
// the whole point.
func TestNonPublishingComposes_LeavePprofOnLoopback(t *testing.T) {
	publishes := map[string]bool{}
	for _, f := range pprofPublishers {
		publishes[f] = true
	}
	widened := regexp.MustCompile(`(?m)^\s*-\s*PPROF_HOST(?:_SERVER|_INGESTOR)?=(?:0\.0\.0\.0|\*|::|\[::\]|\$\{PPROF_HOST(?:_SERVER|_INGESTOR)?:-[^}]+\})\s*$`)
	for _, target := range appComposeServices {
		if publishes[target.file] {
			continue
		}
		t.Run(target.file, func(t *testing.T) {
			ports := extractSubBlock(extractServiceBlock(stripYAMLComments(readRepoCompose(t, target.file)), target.service), "ports", 4)
			if regexp.MustCompile(`606[01]`).MatchString(ports) {
				t.Fatalf("%s now publishes a pprof port; add it to pprofPublishers", target.file)
			}
			env := envBlockOf(t, target.file, target.service)
			if m := widened.FindString(env); m != "" {
				t.Errorf("%s publishes no pprof port but widens the in-container listener: %q", target.file, strings.TrimSpace(m))
			}
		})
	}
}

func TestDeploymentDocs_CarryThe441UpgradeNote(t *testing.T) {
	full := readRepoFile(t, "docs/deployment.md")
	heading := regexp.MustCompile(`(?m)^#{2,4} .*#441.*$`)
	loc := heading.FindStringIndex(full)
	if loc == nil {
		t.Fatalf("docs/deployment.md must carry an upgrade note headed with #441")
	}
	doc := full[loc[0]:]
	if next := regexp.MustCompile(`(?m)^#{2,3} `).FindStringIndex(doc[3:]); next != nil {
		doc = doc[:next[0]+3]
	}
	for _, want := range []string{
		"DISABLE_MOSQUITTO=false", // how to keep the bundled broker
		"PPROF_BIND=0.0.0.0",      // how to keep remote pprof on a published port
		"PPROF_HOST=0.0.0.0",      // ... and on a process with no published port
		"ssh -L",                  // the tunnel alternative
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the #441 upgrade note must name %q; section was:\n%s", want, doc)
		}
	}
}
