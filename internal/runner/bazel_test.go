package runner

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	flagpb "github.com/bazel-contrib/bazelrc-parser-go/bazel_protos/bazel_flags"
	"github.com/buildkite/test-engine-client/v2/internal/bep"
	"github.com/buildkite/test-engine-client/v2/internal/plan"
	"google.golang.org/protobuf/proto"
)

func TestBazelResultPath(t *testing.T) {
	testCases := []struct {
		name   string
		config RunnerConfig
		want   string
	}{
		{
			name:   "configured path",
			config: RunnerConfig{ResultPath: "results/bep.pb"},
			want:   "results/bep.pb",
		},
		{
			name:   "Bazel flag",
			config: RunnerConfig{BazelFlags: "--build_event_binary_file=existing.pb"},
			want:   "existing.pb",
		},
		{
			name:   "separate Bazel flag value",
			config: RunnerConfig{BazelFlags: "--build_event_binary_file 'existing events.pb'"},
			want:   "existing events.pb",
		},
		{
			name:   "last Bazel flag wins",
			config: RunnerConfig{BazelFlags: "--build_event_binary_file=first.pb --build_event_binary_file=last.pb"},
			want:   "last.pb",
		},
		{
			name:   "result path overrides Bazel flag",
			config: RunnerConfig{ResultPath: "override.pb", BazelFlags: "--build_event_binary_file=existing.pb"},
			want:   "override.pb",
		},
		{
			name: "temporary path",
			want: filepath.Join(os.TempDir(), fmt.Sprintf("bktec-bep-%d.pb", os.Getpid())),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			testRunner := NewBazel(testCase.config)
			if got := testRunner.ResultFilePath(); got != testCase.want {
				t.Errorf("ResultFilePath() = %q, want %q", got, testCase.want)
			}
			if got := testRunner.ResultFormat(); got != "bazel-bep" {
				t.Errorf("ResultFormat() = %q, want bazel-bep", got)
			}
		})
	}
}

func TestBazelResultPathEnvironmentWarning(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		env         string
		wantWarning bool
	}{
		{name: "environment override", env: "custom.pb", wantWarning: true},
		{name: "CLI path without environment"},
		{name: "CLI path overrides environment", env: "unused.pb"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("BUILDKITE_TEST_ENGINE_RESULT_PATH", testCase.env)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			stderr := os.Stderr
			os.Stderr = writer
			defer func() {
				os.Stderr = stderr
				_ = writer.Close()
			}()
			NewBazel(RunnerConfig{ResultPath: "custom.pb"})
			_ = writer.Close()
			os.Stderr = stderr
			output, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(output), "Warning: BUILDKITE_TEST_ENGINE_RESULT_PATH overrides Bazel's BEP destination"); got != testCase.wantWarning {
				t.Errorf("warning = %q, want warning: %v", output, testCase.wantWarning)
			}
		})
	}
}

func TestBazelCommandNameAndArgs(t *testing.T) {
	b := NewBazel(RunnerConfig{
		BazelFlags: "--config=ci --build_tests_only",
		ResultPath: "/tmp/bep.pb",
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

func TestBazelInheritedBEPPath(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	flagData, err := proto.Marshal(&flagpb.FlagCollection{FlagInfos: []*flagpb.FlagInfo{
		{Name: proto.String("announce_rc"), RequiresValue: proto.Bool(false)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("bazel", []byte("#!/bin/sh\nprintf '%s' '"+base64.StdEncoding.EncodeToString(flagData)+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"MODULE.bazel": "module(name = \"test\")\n",
		".bazelrc":     "import %workspace%/ci.bazelrc\ncommon --announce_rc\ncommon --build_event_binary_file=common.pb\nbuild --build_event_binary_file=build.pb\ntest --build_event_binary_file=test.pb\ntest:wrapper --config=ci\n",
		"ci.bazelrc":   "test:ci --build_event_binary_file=ci.pb\ntest:ci --test_output=errors\n",
	} {
		if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct {
		name         string
		config       RunnerConfig
		want         string
		wantOverride bool
	}{
		{name: "test defaults", want: "test.pb"},
		{name: "imported config", config: RunnerConfig{BazelFlags: "--config=ci --nocache_test_results"}, want: "ci.pb"},
		{name: "separate config value", config: RunnerConfig{BazelFlags: "--config ci"}, want: "ci.pb"},
		{name: "nested config", config: RunnerConfig{BazelFlags: "--config=wrapper"}, want: "ci.pb"},
		{name: "explicit Bazel flag", config: RunnerConfig{BazelFlags: "--config=ci --build_event_binary_file=flag.pb"}, want: "flag.pb"},
		{name: "explicit result path", config: RunnerConfig{BazelFlags: "--config=ci", ResultPath: "override.pb"}, want: "override.pb", wantOverride: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testRunner := NewBazel(testCase.config)
			if got := testRunner.ResultFilePath(); got != testCase.want {
				t.Fatalf("ResultFilePath() = %q, want %q", got, testCase.want)
			}
			_, args, err := testRunner.CommandNameAndArgs([]plan.TestCase{{Path: "//:test"}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if added := bazelBEPPath(args) != ""; added != (testCase.wantOverride || testCase.want == "flag.pb") {
				t.Errorf("unexpected BEP flag in arguments: %v", args)
			}
		})
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
