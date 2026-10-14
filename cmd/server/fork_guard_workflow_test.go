// Tests for the fork side-effect guards in the CI workflows.
//
// These tests gate workflow config (not Go code) by reading the YAML as text,
// following the same "config gate" pattern as release_fast_path_workflow_test.go.
//
// The workflows are shared with the upstream repository Kpa-clawbot/CoreScope,
// where they publish images and releases, deploy staging on a self-hosted
// runner and commit badge files. On a fork those side effects must never run,
// while ordinary CI (tests, the local image build, CI artifact uploads) keeps
// running. Every guard is ANDed with the job's or step's existing condition, so
// upstream behaviour is unchanged.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const upstreamRepoGuard = "github.repository == 'Kpa-clawbot/CoreScope'"

func readWorkflow(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// workflowJob returns the text of a top-level job (two-space indented key)
// up to the next top-level job.
func workflowJob(t *testing.T, src, job string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^  ` + regexp.QuoteMeta(job) + `:[ \t]*\n(.*?)(?:^  [a-zA-Z0-9_-]+:[ \t]*\n|\z)`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("job %q not found", job)
	}
	return m[1]
}

// jobIf returns the job-level `if:` expression (single or multi-line), or "".
func jobIf(job string) string {
	m := regexp.MustCompile(`(?ms)^    if:(.*?)^    [a-zA-Z-]+:`).FindStringSubmatch(job)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestForkGuardOnSideEffectJobs(t *testing.T) {
	deploy := readWorkflow(t, deployWorkflowRel)
	fastPath := readWorkflow(t, fastPathWorkflowRel)

	cases := []struct {
		file, src, job string
		keep           []string // pre-existing conditions that must survive
	}{
		{"deploy.yml", deploy, "release-artifacts", []string{"startsWith(github.ref, 'refs/tags/v')"}},
		{"deploy.yml", deploy, "deploy", []string{
			"(github.event_name == 'push' || github.event_name == 'workflow_dispatch')",
			"github.ref == 'refs/heads/master'",
		}},
		{"deploy.yml", deploy, "publish", []string{"github.event_name == 'push'"}},
		{"release-fast-path.yml", fastPath, "retag-or-fallback", nil},
	}
	for _, c := range cases {
		cond := jobIf(workflowJob(t, c.src, c.job))
		if !strings.Contains(cond, upstreamRepoGuard) {
			t.Errorf("%s job %q: job-level if must contain %q so it never runs on a fork; got %q", c.file, c.job, upstreamRepoGuard, cond)
		}
		for _, k := range c.keep {
			if !strings.Contains(cond, k) {
				t.Errorf("%s job %q: existing condition %q must be kept (guard is ANDed, not a replacement); got %q", c.file, c.job, k, cond)
			}
		}
	}
}

func TestForkGuardOnPublishSteps(t *testing.T) {
	workflow := readWorkflow(t, deployWorkflowRel)
	imageJob := workflowJob(t, workflow, "image-check")
	if cond := jobIf(imageJob); cond != "" {
		t.Errorf("image-check must stay unconditional so the local image build validates on forks; got if %q", cond)
	}
	if !strings.Contains(imageJob, "Build Go Docker image (local staging)") {
		t.Error("image-check: local staging image build step not found")
	}

	job := workflowJob(t, workflow, "build-and-publish")
	// !cancelled() runs the gate after failed or skipped prerequisites. A
	// cancelled run reports the gate as cancelled, which does not pass.
	if cond := strings.TrimSpace(jobIf(job)); cond != "${{ !cancelled() }}" {
		t.Errorf("build-and-publish must run as a fork-safe fail-closed gate, not skip after test failures; want if ${{ !cancelled() }}, got %q", cond)
	}
	steps := regexp.MustCompile(`(?m)^      - name:`).Split(job, -1)[1:]
	publishMarkers := []string{"docker/setup-buildx-action", "docker/setup-qemu-action", "docker/login-action", "docker/metadata-action", "docker/build-push-action"}
	guarded := 0
	for _, step := range steps {
		ifLine := regexp.MustCompile(`(?m)^        if:(.*)$`).FindStringSubmatch(step)
		cond := ""
		if ifLine != nil {
			cond = ifLine[1]
		}
		for _, marker := range publishMarkers {
			if strings.Contains(step, marker) {
				if !strings.Contains(cond, upstreamRepoGuard) || !strings.Contains(cond, "github.event_name == 'push'") {
					t.Errorf("publish step using %s must require both push and %q; got if %q", marker, upstreamRepoGuard, cond)
				}
				guarded++
			}
		}
	}
	if guarded != len(publishMarkers) {
		t.Errorf("build-and-publish: expected %d guarded publish steps, found %d", len(publishMarkers), guarded)
	}
}

// publishSurfaceMarkers are actions or commands that log in to, or push to, a
// registry. Anywhere in the workflow they are a side effect that must be
// fork-guarded, not only inside build-and-publish.
var publishSurfaceMarkers = []string{
	"docker/login-action", "docker/build-push-action", "docker push", "docker login",
	"docker buildx imagetools", "push: true",
}

func workflowJobNames(t *testing.T, src string) []string {
	t.Helper()
	jobs := regexp.MustCompile(`(?ms)^jobs:[ \t]*\n(.*)`).FindStringSubmatch(src)
	if jobs == nil {
		t.Fatal("jobs: section not found")
	}
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_-]+):[ \t]*$`).FindAllStringSubmatch(jobs[1], -1) {
		names = append(names, m[1])
	}
	return names
}

// Every job, not only build-and-publish, must fork-guard registry login/push
// steps: either at job level or on the step itself.
func TestForkGuardScansEveryJobForPublishSteps(t *testing.T) {
	for _, rel := range []string{deployWorkflowRel, fastPathWorkflowRel} {
		src := readWorkflow(t, rel)
		names := workflowJobNames(t, src)
		if len(names) == 0 || (rel == deployWorkflowRel && len(names) < 5) {
			t.Fatalf("%s: job parsing found too few jobs: %v", rel, names)
		}
		for _, name := range names {
			job := workflowJob(t, src, name)
			jobGuarded := strings.Contains(jobIf(job), upstreamRepoGuard)
			for _, step := range regexp.MustCompile(`(?m)^      - `).Split(job, -1)[1:] {
				ifLine := regexp.MustCompile(`(?m)^        if:(.*)$`).FindStringSubmatch(step)
				stepGuarded := ifLine != nil && strings.Contains(ifLine[1], upstreamRepoGuard)
				for _, marker := range publishSurfaceMarkers {
					if strings.Contains(step, marker) && !jobGuarded && !stepGuarded {
						t.Errorf("%s job %q: step using %q must be fork-guarded (job or step if)", rel, name, marker)
					}
				}
			}
		}
	}
}

// image-check only builds locally. It must not inherit packages: write, and a
// hung Docker build must not hold the required gate pending for the 6 h default.
func TestImageCheckLeastPrivilegeAndTimeout(t *testing.T) {
	job := workflowJob(t, readWorkflow(t, deployWorkflowRel), "image-check")
	perms := regexp.MustCompile(`(?m)^    permissions:[ \t]*\n((?:^      .*\n)+)`).FindStringSubmatch(job)
	if perms == nil {
		t.Fatal("image-check must declare job-level permissions")
	}
	if strings.TrimSpace(perms[1]) != "contents: read" {
		t.Errorf("image-check permissions must be exactly contents: read; got %q", strings.TrimSpace(perms[1]))
	}
	m := regexp.MustCompile(`(?m)^    timeout-minutes:\s*(\d+)\s*$`).FindStringSubmatch(job)
	if m == nil {
		t.Fatal("image-check must set job-level timeout-minutes")
	}
	if n, _ := strconv.Atoi(m[1]); n < 10 || n > 30 {
		t.Errorf("image-check timeout-minutes should be about 20 (10..30); got %d", n)
	}
}

func TestForkGuardLeavesTestJobsRunning(t *testing.T) {
	deploy := readWorkflow(t, deployWorkflowRel)
	var manifest struct {
		Shards int `json:"shards"`
	}
	if err := json.Unmarshal([]byte(readWorkflow(t, "../../scripts/go-server-shards.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Shards < 3 || manifest.Shards > 4 {
		t.Fatalf("Go shard count must be measured and between 3 and 4; got %d", manifest.Shards)
	}
	goJobs := []string{"go-shards-check"}
	goKeys := []string{"GO_GUARD_RESULT"}
	for n := 1; n <= manifest.Shards; n++ {
		goJobs = append(goJobs, fmt.Sprintf("go-server-%d", n))
		goKeys = append(goKeys, fmt.Sprintf("GO_SERVER_%d_RESULT", n))
		job := workflowJob(t, deploy, fmt.Sprintf("go-server-%d", n))
		if !strings.Contains(job, fmt.Sprintf("      GO_SERVER_SHARD: '%d'\n", n)) {
			t.Errorf("server shard %d must have its pinned identity", n)
		}
		if n > 1 && !strings.HasSuffix(strings.TrimSpace(job), "steps: *go-server-shard-steps") {
			t.Errorf("server shard %d must share the same steps without overrides", n)
		}
	}
	first := workflowJob(t, deploy, "go-server-1")
	for _, want := range []string{"steps: &go-server-shard-steps", "node scripts/run-go-server-shard.js\n", "name: go-server-${{ env.GO_SERVER_SHARD }}", "overwrite: true"} {
		if !strings.Contains(first, want) {
			t.Errorf("Go shard runner must preserve %q", want)
		}
	}
	guard := workflowJob(t, deploy, "go-shards-check")
	for _, want := range []string{"node scripts/run-go-server-shard.js --validate", "--runtime-list"} {
		if !strings.Contains(guard, want) {
			t.Errorf("Go assignment guard must include %q", want)
		}
	}
	goJobs = append(goJobs, "go-ingestor", "go-libraries", "js-test", "script-test")
	goKeys = append(goKeys, "GO_INGESTOR_RESULT", "GO_LIBRARIES_RESULT", "JS_RESULT", "SCRIPTS_RESULT")
	for _, name := range append(append([]string{}, goJobs...), "e2e-test-1", "e2e-test-2", "e2e-test-3", "image-check") {
		job := workflowJob(t, deploy, name)
		if cond := jobIf(job); cond != "" {
			t.Errorf("%s must not gain a job-level condition; got %q", name, cond)
		}
		if strings.Contains(job, "github.repository") {
			t.Errorf("%s must not be repository-guarded: tests and CI artifact uploads run on forks too", name)
		}
		if regexp.MustCompile(`(?m)^    needs:`).MatchString(job) {
			t.Errorf("%s must start independently", name)
		}
	}
	// Execute both real gate loops. Static needs/condition checks alone cannot
	// catch a loop which silently stops checking one prerequisite or accepts skips.
	for _, c := range []struct {
		job, step, nextStep string
		needs, keys         []string
	}{
		{"go-test", "Require all Go and split checks", "Checkout code", goJobs, goKeys},
		{"build-and-publish", "Require tests and image validation", "Checkout code",
			append(append([]string{}, goJobs...), "go-test", "e2e-test-1", "e2e-test-2", "e2e-test-3", "e2e-report", "image-check"),
			append(append([]string{}, goKeys...), "GO_RESULT", "E2E_1_RESULT", "E2E_2_RESULT", "E2E_3_RESULT", "E2E_REPORT_RESULT", "IMAGE_RESULT")},
	} {
		job := workflowJob(t, deploy, c.job)
		if strings.TrimSpace(jobIf(job)) != "${{ !cancelled() }}" {
			t.Errorf("%s must run fail-closed after failed or skipped prerequisites", c.job)
		}
		if !strings.Contains(job, "    needs: ["+strings.Join(c.needs, ", ")+"]\n") {
			t.Errorf("%s must require exactly every prerequisite: %v", c.job, c.needs)
		}
		re := regexp.MustCompile(`(?s)      - name: ` + regexp.QuoteMeta(c.step) + `\n(.*?)      - name: ` + regexp.QuoteMeta(c.nextStep))
		gate := re.FindStringSubmatch(job)
		if gate == nil {
			t.Fatalf("%s explicit prerequisite step missing", c.job)
		}
		for i, key := range c.keys {
			if !strings.Contains(gate[1], key+": ${{ needs."+c.needs[i]+".result }}") {
				t.Errorf("%s missing correct result binding for %s", c.job, key)
			}
		}
		run := regexp.MustCompile(`(?m)^        run: \|\n((?:^          .*\n)+)`).FindStringSubmatch(gate[1])
		if run == nil {
			t.Fatalf("%s gate script missing", c.job)
		}
		bash := regexp.MustCompile(`(?m)^          `).ReplaceAllString(run[1], "")
		check := func(badKey, value string) {
			t.Helper()
			cmd := exec.Command("bash", "-e", "-c", bash)
			cmd.Env = os.Environ()
			for _, key := range c.keys {
				result := "success"
				if key == badKey {
					result = value
				}
				cmd.Env = append(cmd.Env, key+"="+result)
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (badKey == "") {
				t.Errorf("%s gate (%s=%q) exit: %v; %s", c.job, badKey, value, err, out)
			}
		}
		check("", "")
		for _, key := range c.keys {
			for _, result := range []string{"failure", "skipped", "cancelled", ""} {
				check(key, result)
			}
		}
	}
	goReport := workflowJob(t, deploy, "go-test")
	if !strings.Contains(goReport, "node scripts/report-go-server-shards.js") || strings.Contains(goReport, "github.repository") {
		t.Error("Go coverage merge must run and must not be repository-guarded")
	}
	report := workflowJob(t, deploy, "e2e-report")
	if cond := strings.TrimSpace(jobIf(report)); cond != "${{ !cancelled() }}" {
		t.Errorf("e2e-report must run after failed or skipped shards; got if %q", cond)
	}
	if strings.Contains(report, "github.repository") {
		t.Error("e2e-report must not be repository-guarded")
	}
}

// The required check is configured by name, so renaming the gate job would
// silently drop the requirement.
func TestRequiredGateCheckNameStable(t *testing.T) {
	job := workflowJob(t, readWorkflow(t, deployWorkflowRel), "build-and-publish")
	const want = `    name: "🏗️ Build & Publish Docker Image"`
	if !strings.Contains(job, want+"\n") {
		t.Errorf("build-and-publish must keep the stable required check name %q", want)
	}
}

// Pins the number of fork guards so one cannot be dropped unnoticed. Comment
// lines are ignored: deploy.yml also names the guard once in its header, so
// `grep -c` reports 9 there. Update the counts only when a guarded side effect
// is deliberately added or removed.
func TestForkGuardCount(t *testing.T) {
	for _, c := range []struct {
		rel  string
		want int
	}{
		{deployWorkflowRel, 8},
		{fastPathWorkflowRel, 1},
	} {
		got := 0
		for _, line := range strings.Split(readWorkflow(t, c.rel), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				got += strings.Count(line, upstreamRepoGuard)
			}
		}
		if got != c.want {
			t.Errorf("%s: expected %d fork guards outside comments, found %d", c.rel, c.want, got)
		}
	}
}

// A single shared steps anchor prevents fixtures or setup from silently
// diverging between shards. The alias jobs may vary only their local port
// and shard identity, never their preparation or suite runner.
func TestE2EShardsShareFixtureSteps(t *testing.T) {
	deploy := readWorkflow(t, deployWorkflowRel)
	first := workflowJob(t, deploy, "e2e-test-1")
	if !strings.Contains(first, "    steps: &e2e-shard-steps\n") {
		t.Fatal("shard 1 must declare the shared E2E steps anchor")
	}
	for _, c := range []struct{ job, shard, port string }{
		{"e2e-test-1", "1", "13581"},
		{"e2e-test-2", "2", "13582"},
		{"e2e-test-3", "3", "13583"},
	} {
		job := workflowJob(t, deploy, c.job)
		for _, want := range []string{
			"      E2E_SHARD: '" + c.shard + "'\n",
			"      E2E_PORT: '" + c.port + "'\n",
			"      BASE_URL: http://localhost:" + c.port + "\n",
			"    timeout-minutes: 150\n",
		} {
			if !strings.Contains(job, want) {
				t.Errorf("%s must contain %q", c.job, want)
			}
		}
		if c.shard != "1" && !strings.HasSuffix(strings.TrimSpace(job), "steps: *e2e-shard-steps") {
			t.Errorf("%s must use the identical shared steps, without overrides", c.job)
		}
	}
	previous := -1
	for _, marker := range []string{
		"bash tools/freshen-fixture.sh test-fixtures/e2e-fixture.db",
		"sqlite3 test-fixtures/e2e-fixture.db <<'SQL'",
		"./corescope-migrate -db test-fixtures/e2e-fixture.db",
		"sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-2073-route-adverts.sql",
		"sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-199-inactive-observer.sql",
		"sqlite3 test-fixtures/e2e-fixture.db < test-fixtures/seed-245-advert-intervals.sql",
		"./corescope-server -port \"$E2E_PORT\" -db test-fixtures/e2e-fixture.db -public public-instrumented &",
		"node scripts/run-e2e-shard.js\n",
	} {
		index := strings.Index(first, marker)
		if index <= previous {
			t.Fatalf("fixture step %q is missing or out of order", marker)
		}
		previous = index
	}
}

func TestE2ECoverageRunsOnCoreShardOnly(t *testing.T) {
	deploy := readWorkflow(t, deployWorkflowRel)
	first := workflowJob(t, deploy, "e2e-test-1")
	if !strings.Contains(first, "node scripts/run-e2e-shard.js --core-shard") {
		t.Error("coverage owner must be identified from the checked-in manifest")
	}
	const condition = "if: success() && github.event_name == 'push' && steps.shard-meta.outputs.core_shard == env.E2E_SHARD"
	if !strings.Contains(first, condition) {
		t.Error("coverage collection must stay push-only and run once on the core suite's shard")
	}
	if strings.Contains(first, "Generate frontend coverage badges") {
		t.Error("per-shard badges must not replace the aggregate badge summary")
	}
	report := workflowJob(t, deploy, "e2e-report")
	for _, need := range []string{"node scripts/report-e2e-shards.js", "Generate frontend coverage badges", "name: e2e-badges"} {
		if !strings.Contains(report, need) {
			t.Errorf("e2e-report must preserve %q", need)
		}
	}
}
