package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/buildkite/test-engine-client/v2/internal/bep"
	"github.com/buildkite/test-engine-client/v2/internal/plan"
)

func TestBazelCommandNameAndArgs(t *testing.T) {
	b := NewBazel(RunnerConfig{
		BazelFlags: "--config=ci --build_tests_only",
		BepPath:    "/tmp/bep.pb",
	})

	testCases := []plan.TestCase{
		{Path: "//foo:bar"},
		{Path: "//foo:bar"}, // duplicate label should be deduped
		{Path: "//baz:qux"},
	}

	name, args, err := b.CommandNameAndArgs(testCases, false)
	if err != nil {
		t.Fatalf("CommandNameAndArgs returned error: %v", err)
	}
	if name != "bazel" {
		t.Errorf("command name = %q, want %q", name, "bazel")
	}

	want := []string{
		"test",
		"--config=ci",
		"--build_tests_only",
		"--build_event_binary_file=/tmp/bep.pb",
		"//foo:bar",
		"//baz:qux",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBazelIsTestFailureExitCode(t *testing.T) {
	b := NewBazel(RunnerConfig{})
	cases := map[int]bool{
		0: false, // success
		1: false, // build failure
		3: true,  // tests failed
		4: false, // no tests found
	}
	for code, want := range cases {
		if got := b.IsTestFailureExitCode(code); got != want {
			t.Errorf("IsTestFailureExitCode(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestBazelRecordTarget_WithXML(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="//foo:bar">
    <testcase classname="foo.Bar" name="test_one"/>
    <testcase classname="foo.Bar" name="test_two"><failure message="boom"/></testcase>
  </testsuite>
</testsuites>`
	dir := t.TempDir()
	xmlPath := filepath.Join(dir, "test.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
		t.Fatalf("failed to write xml: %v", err)
	}

	b := NewBazel(RunnerConfig{})
	result := NewRunResult([]plan.TestCase{})
	b.recordTarget(result, bep.TargetResult{
		Label:    "//foo:bar",
		Status:   bep.StatusFailed,
		XMLPaths: []string{xmlPath},
	})

	stats := result.Statistics()
	if stats.Total != 2 {
		t.Fatalf("expected 2 recorded tests, got %d", stats.Total)
	}

	// scope should be the label, name should be the testcase name
	failed := result.FailedTests()
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed test, got %d", len(failed))
	}
	if failed[0].Scope != "//foo:bar" {
		t.Errorf("failed test scope = %q, want %q", failed[0].Scope, "//foo:bar")
	}
	if failed[0].Name != "test_two" {
		t.Errorf("failed test name = %q, want %q", failed[0].Name, "test_two")
	}
}

func TestBazelRecordTarget_NoXML(t *testing.T) {
	b := NewBazel(RunnerConfig{})
	result := NewRunResult([]plan.TestCase{})

	// A target that failed to build emits no JUnit XML; a single target-level
	// result should be recorded with scope == name == label.
	b.recordTarget(result, bep.TargetResult{
		Label:  "//foo:broken",
		Status: bep.StatusFailed,
	})

	failed := result.FailedTests()
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed test, got %d", len(failed))
	}
	if failed[0].Scope != "//foo:broken" || failed[0].Name != "//foo:broken" {
		t.Errorf("got scope=%q name=%q, want both %q", failed[0].Scope, failed[0].Name, "//foo:broken")
	}
}

func TestBazelRecordTarget_MutedFailureSuppressed(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="//foo:bar">
    <testcase classname="foo.Bar" name="flaky_one"><failure message="boom"/></testcase>
  </testsuite>
</testsuites>`
	dir := t.TempDir()
	xmlPath := filepath.Join(dir, "test.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
		t.Fatalf("failed to write xml: %v", err)
	}

	b := NewBazel(RunnerConfig{})
	// Mute is keyed by scope/name == label/testcase.
	result := NewRunResult([]plan.TestCase{
		{Scope: "//foo:bar", Name: "flaky_one"},
	})
	b.recordTarget(result, bep.TargetResult{
		Label:    "//foo:bar",
		Status:   bep.StatusFailed,
		XMLPaths: []string{xmlPath},
	})

	if !result.OnlyMutedFailures() {
		t.Errorf("expected OnlyMutedFailures to be true for a muted-only failure")
	}
	if len(result.FailedTests()) != 0 {
		t.Errorf("muted failure should not be counted as a hard failure")
	}
}
