package runner

import "github.com/buildkite/test-engine-client/v2/internal/plan"

type TestRunner interface {
	Run(result *RunResult, testCases []plan.TestCase, retry bool) error
	GetExamples(files []string) ([]plan.TestCase, error)
	GetFiles() ([]string, error)
	Name() string
	CommandNameAndArgs(testCases []plan.TestCase, retry bool) (string, []string, error)
	LocationPrefix() string
	SupportedFeatures() SupportedFeatures
	UploadToken() string
	// ResultFormat returns the format identifier for the runner's raw result file
	// (e.g. "rspec-json"), used when uploading results to Test Engine.
	// Returns an empty string if the runner does not support raw result uploads.
	ResultFormat() string
	// ResultFilePath returns the path to the runner's raw result file.
	ResultFilePath() string
}

// TestFailureExitCoder is an optional interface a runner can implement to tell
// bktec which process exit codes represent test failures (as opposed to build
// or usage errors). When implemented, it is used to decide whether a non-zero
// exit can be suppressed because only muted tests failed. Runners that don't
// implement it fall back to treating exit code 1 as a test failure.
type TestFailureExitCoder interface {
	IsTestFailureExitCode(code int) bool
}
