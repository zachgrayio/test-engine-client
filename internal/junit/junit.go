// Package junit parses JUnit XML test reports into a minimal, runner-agnostic
// representation. It is shared by the Bazel runner (which records results into
// a RunResult) and the BEP ingest path (which builds Test Engine upload JSON),
// so both derive identical test identities from the same XML.
package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

// Status is the outcome of a single test case.
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

// TestCase is a single <testcase> element.
type TestCase struct {
	Classname string
	Name      string
	Result    Status
	Duration  float64
}

type xmlFailure struct {
	Message string `xml:"message,attr"`
}

type xmlTestCase struct {
	Classname string      `xml:"classname,attr"`
	Name      string      `xml:"name,attr"`
	Time      float64     `xml:"time,attr"`
	Failure   *xmlFailure `xml:"failure"`
	Error     *xmlFailure `xml:"error"`
	Skipped   *xmlFailure `xml:"skipped"`
}

type xmlTestSuite struct {
	Name      string        `xml:"name,attr"`
	TestCases []xmlTestCase `xml:"testcase"`
}

type xmlTestSuites struct {
	XMLName    xml.Name       `xml:"testsuites"`
	TestSuites []xmlTestSuite `xml:"testsuite"`
}

// Parse reads a JUnit XML file and returns its test cases. It accepts both a
// <testsuites> root and a single <testsuite> root.
func Parse(path string) ([]TestCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open JUnit XML file %s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read JUnit XML file %s: %w", path, err)
	}

	var suites xmlTestSuites
	suitesErr := xml.Unmarshal(data, &suites)

	// Some rules emit a single <testsuite> root rather than <testsuites>.
	// The <testsuites> unmarshal returns an error in that case (root element
	// mismatch), so fall back whenever we got no suites.
	if suitesErr != nil || len(suites.TestSuites) == 0 {
		var single xmlTestSuite
		if err := xml.Unmarshal(data, &single); err == nil && (len(single.TestCases) > 0 || single.Name != "") {
			suites.TestSuites = []xmlTestSuite{single}
		} else if suitesErr != nil {
			return nil, fmt.Errorf("failed to unmarshal JUnit XML file %s: %w", path, suitesErr)
		}
	}

	var results []TestCase
	for _, suite := range suites.TestSuites {
		for _, tc := range suite.TestCases {
			results = append(results, TestCase{
				Classname: tc.Classname,
				Name:      tc.Name,
				Result:    statusFor(tc),
				Duration:  tc.Time,
			})
		}
	}
	return results, nil
}

func statusFor(tc xmlTestCase) Status {
	switch {
	case tc.Failure != nil || tc.Error != nil:
		return StatusFailed
	case tc.Skipped != nil:
		return StatusSkipped
	default:
		return StatusPassed
	}
}
