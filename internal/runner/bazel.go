package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bazel-contrib/bazelrc-parser-go/bazelrc"
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
	bepPath       string
	bepConfigured bool
	bepError      error
}

// Compile-time check that Bazel implements TestRunner.
var _ TestRunner = (*Bazel)(nil)

// Compile-time check that Bazel reports its own test-failure exit code.
var _ TestFailureExitCoder = (*Bazel)(nil)

func NewBazel(c RunnerConfig) Bazel {
	if c.MaxRetries > 0 {
		fmt.Fprintf(os.Stderr, "Buildkite Test Engine Client: Warning: bktec retries are enabled (%d retries); overriding Bazel --flaky_test_attempts=1 so only bktec retries failed tests\n", c.MaxRetries)
	}
	bepPath := c.ResultPath
	if bepPath != "" && os.Getenv("BUILDKITE_TEST_ENGINE_RESULT_PATH") == bepPath {
		fmt.Fprintf(os.Stderr, "Buildkite Test Engine Client: Warning: BUILDKITE_TEST_ENGINE_RESULT_PATH overrides Bazel's BEP destination with %q\n", bepPath)
	}
	bepConfigured := false
	var bepError error
	if bepPath == "" {
		flags, err := shellquote.Split(c.BazelFlags)
		if err == nil {
			bepPath, bepError = configuredBazelBEPPath(flags)
			bepConfigured = bepPath != ""
		} else {
			bepError = err
		}
	}
	if bepPath == "" {
		bepPath = filepath.Join(os.TempDir(), fmt.Sprintf("bktec-bep-%d.pb", os.Getpid()))
	}

	return Bazel{
		RunnerConfig:  c,
		bepPath:       bepPath,
		bepConfigured: bepConfigured,
		bepError:      bepError,
	}
}

func configuredBazelBEPPath(flags []string) (string, error) {
	if path := bazelBEPPath(flags); path != "" {
		return path, nil
	}
	workspace, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for directory := workspace; ; directory = filepath.Dir(directory) {
		found := false
		for _, marker := range []string{"MODULE.bazel", "WORKSPACE.bazel", "WORKSPACE"} {
			if _, err := os.Stat(filepath.Join(directory, marker)); err == nil {
				workspace = directory
				found = true
				break
			}
		}
		if found || filepath.Dir(directory) == directory {
			break
		}
	}
	rcFiles := []string{"/etc/bazel.bazelrc", filepath.Join(workspace, ".bazelrc")}
	if home, err := os.UserHomeDir(); err == nil {
		rcFiles = append(rcFiles, filepath.Join(home, ".bazelrc"))
	}
	var imports strings.Builder
	hasRCFile := false
	for _, path := range rcFiles {
		if _, err := os.Stat(path); err == nil {
			hasRCFile = true
		}
		imports.WriteString(shellquote.Join("try-import", path))
		imports.WriteByte('\n')
	}
	if !hasRCFile {
		return "", nil
	}
	flagData, err := bazelrc.GetFlagDataFromBazel(exec.Command("bazel"))
	if err != nil {
		return "", fmt.Errorf("read Bazel flag metadata: %w", err)
	}
	contents, err := bazelrc.NewBazelRcParser(workspace, flagData).Parsefile(strings.NewReader(imports.String()), ".bazelrc")
	if err != nil {
		return "", fmt.Errorf("resolve Bazel BEP configuration: %w", err)
	}
	var path string
	active := make(map[string]bool)
	var apply func(string) error
	apply = func(config string) error {
		if active[config] {
			return fmt.Errorf("Bazel config cycle involving %q", config)
		}
		active[config] = true
		defer delete(active, config)
		for _, command := range []string{"common", "build", "test"} {
			if config != "" {
				command += ":" + config
			}
			if value := contents.FlagValue(command, "build_event_binary_file"); value != nil {
				path = *value
			}
			if nested := contents.FlagValue(command, "config"); nested != nil {
				if err := apply(*nested); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := apply(""); err != nil {
		return "", err
	}
	for index := 0; index < len(flags); index++ {
		flag := flags[index]
		if flag == "--" {
			break
		}
		var config string
		if strings.HasPrefix(flag, "--config=") {
			config = strings.TrimPrefix(flag, "--config=")
		} else if flag == "--config" && index+1 < len(flags) {
			index++
			config = flags[index]
		} else {
			continue
		}
		if err := apply(config); err != nil {
			return "", err
		}
	}
	return path, nil
}

func bazelBEPPath(flags []string) string {
	var path string
	for index := 0; index < len(flags); index++ {
		flag := flags[index]
		if flag == "--" {
			break
		}
		if strings.HasPrefix(flag, "--build_event_binary_file=") {
			path = strings.TrimPrefix(flag, "--build_event_binary_file=")
		} else if flag == "--build_event_binary_file" && index+1 < len(flags) {
			index++
			path = flags[index]
		}
	}
	return path
}

func (b Bazel) Name() string {
	return "bazel"
}

func (b Bazel) ResultFormat() string {
	return "bazel-bep"
}

func (b Bazel) ResultFilePath() string {
	return b.bepPath
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
	if b.bepError != nil {
		return nil, b.bepError
	}
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
	if b.bepError != nil {
		return "", nil, b.bepError
	}
	labels := b.getLabels(testCases)

	args := []string{"test"}

	if b.BazelFlags != "" {
		flags, err := shellquote.Split(b.BazelFlags)
		if err != nil {
			return "", nil, fmt.Errorf("failed to parse BUILDKITE_TEST_ENGINE_BAZEL_FLAGS: %w", err)
		}
		args = append(args, flags...)
	}

	if !b.bepConfigured {
		args = append(args, fmt.Sprintf("--build_event_binary_file=%s", b.bepPath))
	}

	if b.MaxRetries > 0 {
		args = append(args, "--flaky_test_attempts=1")
	}

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
