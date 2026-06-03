package runner

import "github.com/buildkite/test-engine-client/v2/internal/junit"

// junitStatusToTestStatus maps a parsed JUnit status (from the shared
// internal/junit parser) to the runner's TestStatus.
func junitStatusToTestStatus(s junit.Status) TestStatus {
	switch s {
	case junit.StatusPassed:
		return TestStatusPassed
	case junit.StatusFailed:
		return TestStatusFailed
	case junit.StatusSkipped:
		return TestStatusSkipped
	default:
		return TestStatusUnknown
	}
}
