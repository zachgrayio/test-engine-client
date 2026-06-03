package bep

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	bes "github.com/aspect-build/aspect-cli/bazel/buildeventstream"
	"google.golang.org/protobuf/encoding/protodelim"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

func findTarget(results []TargetResult, label string) *TargetResult {
	for i := range results {
		if results[i].Label == label {
			return &results[i]
		}
	}
	return nil
}

func TestParseFile_JSON(t *testing.T) {
	jsonBEP := `
{"id":{"testResult":{"label":"//foo:bar","run":1,"shard":1,"attempt":1}},"testResult":{"testActionOutput":[{"name":"test.xml","uri":"file:///tmp/foo/test.xml"},{"name":"test.log","uri":"file:///tmp/foo/test.log"}],"status":"PASSED","cachedLocally":false}}
{"id":{"testSummary":{"label":"//foo:bar"}},"testSummary":{"overallStatus":"PASSED","totalRunCount":1}}
{"id":{"testResult":{"label":"//baz:qux","run":1,"shard":1,"attempt":1}},"testResult":{"testActionOutput":[{"name":"test.xml","uri":"file:///tmp/baz/test.xml"}],"status":"FAILED"}}
{"id":{"testSummary":{"label":"//baz:qux"}},"testSummary":{"overallStatus":"FAILED","totalRunCount":1}}
`
	path := writeTemp(t, "bep.json", []byte(jsonBEP))

	results, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(results))
	}

	foo := findTarget(results, "//foo:bar")
	if foo == nil {
		t.Fatal("missing //foo:bar")
	}
	if foo.Status != StatusPassed {
		t.Errorf("//foo:bar status = %q, want %q", foo.Status, StatusPassed)
	}
	if len(foo.XMLPaths) != 1 || foo.XMLPaths[0] != "/tmp/foo/test.xml" {
		t.Errorf("//foo:bar XMLPaths = %v, want [/tmp/foo/test.xml]", foo.XMLPaths)
	}

	baz := findTarget(results, "//baz:qux")
	if baz == nil {
		t.Fatal("missing //baz:qux")
	}
	if baz.Status != StatusFailed {
		t.Errorf("//baz:qux status = %q, want %q", baz.Status, StatusFailed)
	}
}

func TestParseFile_JSON_FlakyOverridesResult(t *testing.T) {
	// First attempt fails, second passes; TestSummary reports FLAKY.
	jsonBEP := `
{"id":{"testResult":{"label":"//foo:flaky","attempt":1}},"testResult":{"status":"FAILED","testActionOutput":[{"name":"test.xml","uri":"file:///tmp/flaky/attempt_1/test.xml"}]}}
{"id":{"testResult":{"label":"//foo:flaky","attempt":2}},"testResult":{"status":"PASSED","testActionOutput":[{"name":"test.xml","uri":"file:///tmp/flaky/test.xml"}]}}
{"id":{"testSummary":{"label":"//foo:flaky"}},"testSummary":{"overallStatus":"FLAKY","totalRunCount":2}}
`
	path := writeTemp(t, "bep.json", []byte(jsonBEP))

	results, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	flaky := findTarget(results, "//foo:flaky")
	if flaky == nil {
		t.Fatal("missing //foo:flaky")
	}
	if flaky.Status != StatusFlaky {
		t.Errorf("status = %q, want %q", flaky.Status, StatusFlaky)
	}
	// Both attempts' XML paths should be captured.
	if len(flaky.XMLPaths) != 2 {
		t.Errorf("XMLPaths = %v, want 2 entries", flaky.XMLPaths)
	}
}

func TestParseFile_Binary(t *testing.T) {
	events := []*bes.BuildEvent{
		{
			Id: &bes.BuildEventId{
				Id: &bes.BuildEventId_TestResult{
					TestResult: &bes.BuildEventId_TestResultId{Label: "//foo:bar"},
				},
			},
			Payload: &bes.BuildEvent_TestResult{
				TestResult: &bes.TestResult{
					Status: bes.TestStatus_PASSED,
					TestActionOutput: []*bes.File{
						{Name: "test.xml", File: &bes.File_Uri{Uri: "file:///tmp/foo/test.xml"}},
					},
				},
			},
		},
		{
			Id: &bes.BuildEventId{
				Id: &bes.BuildEventId_TestSummary{
					TestSummary: &bes.BuildEventId_TestSummaryId{Label: "//foo:bar"},
				},
			},
			Payload: &bes.BuildEvent_TestSummary{
				TestSummary: &bes.TestSummary{OverallStatus: bes.TestStatus_PASSED},
			},
		},
	}

	var buf bytes.Buffer
	for _, ev := range events {
		if _, err := protodelim.MarshalTo(&buf, ev); err != nil {
			t.Fatalf("failed to marshal event: %v", err)
		}
	}
	path := writeTemp(t, "bep.pb", buf.Bytes())

	results, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	foo := findTarget(results, "//foo:bar")
	if foo == nil {
		t.Fatal("missing //foo:bar")
	}
	if foo.Status != StatusPassed {
		t.Errorf("status = %q, want %q", foo.Status, StatusPassed)
	}
	if len(foo.XMLPaths) != 1 || foo.XMLPaths[0] != "/tmp/foo/test.xml" {
		t.Errorf("XMLPaths = %v, want [/tmp/foo/test.xml]", foo.XMLPaths)
	}
}

func TestUriToPath(t *testing.T) {
	cases := map[string]string{
		"file:///tmp/foo/test.xml": "/tmp/foo/test.xml",
		"bytestream://remote/abc":  "",
		"":                         "",
	}
	for uri, want := range cases {
		if got := uriToPath(uri); got != want {
			t.Errorf("uriToPath(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestMapStatus(t *testing.T) {
	cases := map[bes.TestStatus]Status{
		bes.TestStatus_PASSED:          StatusPassed,
		bes.TestStatus_FLAKY:           StatusFlaky,
		bes.TestStatus_FAILED:          StatusFailed,
		bes.TestStatus_TIMEOUT:         StatusFailed,
		bes.TestStatus_FAILED_TO_BUILD: StatusFailed,
		bes.TestStatus_NO_STATUS:       StatusUnknown,
	}
	for in, want := range cases {
		if got := mapStatus(in); got != want {
			t.Errorf("mapStatus(%v) = %q, want %q", in, got, want)
		}
	}
}
