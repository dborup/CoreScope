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
	"os"
	"regexp"
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
	job := workflowJob(t, readWorkflow(t, deployWorkflowRel), "build-and-publish")
	if cond := jobIf(job); cond != "" {
		t.Errorf("build-and-publish must stay unconditional at job level so the local image build still validates on forks; got if %q", cond)
	}
	steps := regexp.MustCompile(`(?m)^      - name:`).Split(job, -1)[1:]
	publishMarkers := []string{"docker/setup-buildx-action", "docker/setup-qemu-action", "docker/login-action", "docker/metadata-action", "docker/build-push-action"}
	guarded, sawLocalBuild := 0, false
	for _, step := range steps {
		ifLine := regexp.MustCompile(`(?m)^        if:(.*)$`).FindStringSubmatch(step)
		cond := ""
		if ifLine != nil {
			cond = ifLine[1]
		}
		if strings.Contains(step, "Build Go Docker image (local staging)") {
			sawLocalBuild = true
			if strings.Contains(cond, "github.repository") {
				t.Errorf("local staging image build must not be repository-guarded (it is fork-safe validation); got if %q", cond)
			}
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
	if !sawLocalBuild {
		t.Errorf("build-and-publish: local staging image build step not found")
	}
	if guarded != len(publishMarkers) {
		t.Errorf("build-and-publish: expected %d guarded publish steps, found %d", len(publishMarkers), guarded)
	}
}

func TestForkGuardLeavesTestJobsRunning(t *testing.T) {
	deploy := readWorkflow(t, deployWorkflowRel)
	for _, name := range []string{"go-test", "e2e-test"} {
		job := workflowJob(t, deploy, name)
		if cond := jobIf(job); cond != "" {
			t.Errorf("%s must not gain a job-level condition; got %q", name, cond)
		}
		if strings.Contains(job, "github.repository") {
			t.Errorf("%s must not be repository-guarded: tests and CI artifact uploads run on forks too", name)
		}
	}
}
