package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/buildkite/test-engine-client/v2/internal/bep"
	"github.com/buildkite/test-engine-client/v2/internal/debug"
	"github.com/buildkite/test-engine-client/v2/internal/junit"
	"github.com/buildkite/test-engine-client/v2/internal/plan"
	"github.com/kballard/go-shellquote"
)

// Bazel is a test runner that invokes `bazel test`, consumes the Build Event
// Protocol (BEP) it emits, and maps the per-target results into Test Engine
// identities (scope = Bazel target label, name = JUnit testcase name).
type Bazel struct {
	RunnerConfig
	// bepPath is the resolved path to the BEP file that bktec asks bazel to
	// write and then parses. Stable across retries (overwritten each attempt).
	bepPath string
}

// Compile-time check that Bazel implements TestRunner.
var _ TestRunner = (*Bazel)(nil)

// Compile-time check that Bazel reports its own test-failure exit code.
var _ TestFailureExitCoder = (*Bazel)(nil)

func NewBazel(c RunnerConfig) Bazel {
	bepPath := c.BepPath
	if bepPath == "" {
		bepPath = filepath.Join(os.TempDir(), fmt.Sprintf("bktec-bep-%d.pb", os.Getpid()))
	}

	return Bazel{
		RunnerConfig: c,
		bepPath:      bepPath,
	}
}

func (b Bazel) Name() string {
	return "bazel"
}

func (b Bazel) SupportedFeatures() SupportedFeatures {
	return SupportedFeatures{
		SplitByFile:     true,
		SplitByExample:  false,
		FilterTestFiles: false,
		FilterTestByTag: false,
		AutoRetry:       true,
		Mute:            true,
		Skip:            true,
	}
}

// GetExamples is not supported; Bazel splits at the target ("file") level.
func (b Bazel) GetExamples(files []string) ([]plan.TestCase, error) {
	return nil, fmt.Errorf("not supported in bazel runner")
}

// GetFiles discovers Bazel test targets using `bazel query`. Each "file" is a
// target label. The pipeline can bypass this by passing a target list via
// --files.
func (b Bazel) GetFiles() ([]string, error) {
	query := b.BazelQuery
	if query == "" {
		query = "tests(//...)"
	}

	debug.Println("Discovering Bazel test targets with query:", query)
	cmd := exec.Command("bazel", "query", query, "--output=label")
	output, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("bazel query failed: %w\nstderr:\n%s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("failed to run bazel query: %w", err)
	}

	labels := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		label := strings.TrimSpace(line)
		if label != "" {
			labels = append(labels, label)
		}
	}

	debug.Println("Discovered", len(labels), "Bazel test targets")
	if len(labels) == 0 {
		return nil, fmt.Errorf("no Bazel test targets found using query %q", query)
	}
	return labels, nil
}

func (b Bazel) CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error) {
	labels := b.getLabels(testCases)

	args := []string{"test"}

	if b.BazelFlags != "" {
		flags, err := shellquote.Split(b.BazelFlags)
		if err != nil {
			return "", nil, fmt.Errorf("failed to parse BUILDKITE_TEST_ENGINE_BAZEL_FLAGS: %w", err)
		}
		args = append(args, flags...)
	}

	// bktec owns the BEP file so it can parse the results. Appended last so it
	// wins over anything a user may have set in BazelFlags.
	args = append(args, fmt.Sprintf("--build_event_binary_file=%s", b.bepPath))

	args = append(args, labels...)

	return "bazel", args, nil
}

// getLabels returns the unique target labels from the test cases (each
// TestCase.Path is a label).
func (b Bazel) getLabels(testCases []plan.TestCase) []string {
	labels := make([]string, 0, len(testCases))
	seen := map[string]bool{}
	for _, tc := range testCases {
		if tc.Path != "" && !seen[tc.Path] {
			labels = append(labels, tc.Path)
			seen[tc.Path] = true
		}
	}
	return labels
}

// Run invokes `bazel test`, then parses the BEP file it produced and records
// per-case results. When there are no targets to run but a BEP file already
// exists (ingest-only mode), it parses that file without invoking bazel.
func (b Bazel) Run(result *RunResult, testCases []plan.TestCase, retry bool) error {
	cmd, err := buildCommand(b, testCases, retry)
	if err != nil {
		return err
	}

	cmdErr := runAndForwardSignal(cmd)

	if _, statErr := os.Stat(b.bepPath); statErr != nil {
		fmt.Printf("Buildkite Test Engine Client: BEP file not found at %s, results will not be processed: %v\n", b.bepPath, statErr)
		return cmdErr
	}

	targets, parseErr := bep.ParseFile(b.bepPath)
	if parseErr != nil {
		fmt.Printf("Buildkite Test Engine Client: Failed to parse Bazel BEP output: %v\n", parseErr)
		// Don't fail the build solely because we couldn't parse the BEP;
		// return the command error (which may be nil).
		return cmdErr
	}

	for _, target := range targets {
		b.recordTarget(result, target)
	}

	return cmdErr
}

// recordTarget maps a single Bazel target's BEP result into Test Engine test
// cases. When the target produced JUnit XML, each <testcase> becomes a result
// with scope = target label. When no local XML is available (e.g. a build
// failure or remote-only outputs), a single target-level result is recorded so
// failures still propagate and target-level mutes can match.
func (b Bazel) recordTarget(result *RunResult, target bep.TargetResult) {
	recorded := false
	for _, xmlPath := range target.XMLPaths {
		cases, err := junit.Parse(xmlPath)
		if err != nil {
			debug.Printf("Failed to parse JUnit XML %s for %s: %v", xmlPath, target.Label, err)
			continue
		}
		for _, tc := range cases {
			result.RecordTestResult(plan.TestCase{
				Format: plan.TestCaseFormatExample,
				Scope:  target.Label,
				Name:   tc.Name,
				Path:   target.Label,
			}, junitStatusToTestStatus(tc.Result))
			recorded = true
		}
	}

	if !recorded {
		result.RecordTestResult(plan.TestCase{
			Format: plan.TestCaseFormatExample,
			Scope:  target.Label,
			Name:   target.Label,
			Path:   target.Label,
		}, bepStatusToTestStatus(target.Status))
	}
}

// IsTestFailureExitCode reports whether a bazel exit code represents test
// failures (as opposed to build errors or usage errors). Bazel uses exit code
// 3 for "tests failed"; 1 is a build failure, 4 is "no tests found". Only test
// failures are eligible for muted-failure suppression.
func (b Bazel) IsTestFailureExitCode(code int) bool {
	return code == 3
}

func bepStatusToTestStatus(s bep.Status) TestStatus {
	switch s {
	case bep.StatusPassed, bep.StatusFlaky:
		return TestStatusPassed
	case bep.StatusFailed:
		return TestStatusFailed
	case bep.StatusSkipped:
		return TestStatusSkipped
	default:
		return TestStatusUnknown
	}
}
