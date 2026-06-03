package junit

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.xml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestParse_TestSuites(t *testing.T) {
	path := write(t, `<?xml version="1.0"?>
<testsuites>
  <testsuite name="//foo:bar">
    <testcase classname="foo.Bar" name="pass_one"/>
    <testcase classname="foo.Bar" name="fail_one"><failure message="boom"/></testcase>
    <testcase classname="foo.Bar" name="skip_one"><skipped/></testcase>
    <testcase classname="foo.Bar" name="err_one"><error message="kaboom"/></testcase>
  </testsuite>
</testsuites>`)

	cases, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cases) != 4 {
		t.Fatalf("expected 4 cases, got %d", len(cases))
	}

	want := map[string]Status{
		"pass_one": StatusPassed,
		"fail_one": StatusFailed,
		"skip_one": StatusSkipped,
		"err_one":  StatusFailed,
	}
	for _, c := range cases {
		if want[c.Name] != c.Result {
			t.Errorf("case %q result = %q, want %q", c.Name, c.Result, want[c.Name])
		}
	}
}

func TestParse_SingleTestSuiteRoot(t *testing.T) {
	path := write(t, `<?xml version="1.0"?>
<testsuite name="//foo:bar">
  <testcase classname="foo.Bar" name="only"/>
</testsuite>`)

	cases, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cases) != 1 || cases[0].Name != "only" || cases[0].Result != StatusPassed {
		t.Fatalf("unexpected cases: %+v", cases)
	}
}
