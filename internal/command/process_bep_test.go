package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/test-engine-client/v2/internal/bep"
)

func TestBepTargetsToUploadResults_WithXML(t *testing.T) {
	xml := `<?xml version="1.0"?>
<testsuites>
  <testsuite name="//foo:bar">
    <testcase classname="foo.Bar" name="test_one"/>
    <testcase classname="foo.Bar" name="test_two"><failure message="boom"/></testcase>
  </testsuite>
</testsuites>`
	xmlPath := filepath.Join(t.TempDir(), "test.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	results := bepTargetsToUploadResults([]bep.TargetResult{
		{Label: "//foo:bar", Status: bep.StatusFailed, XMLPaths: []string{xmlPath}},
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Scope != "//foo:bar" {
			t.Errorf("scope = %q, want //foo:bar", r.Scope)
		}
		if r.History.Section == "" {
			t.Errorf("history.section should be set")
		}
	}
}

func TestBepTargetsToUploadResults_NoXML(t *testing.T) {
	results := bepTargetsToUploadResults([]bep.TargetResult{
		{Label: "//foo:broken", Status: bep.StatusFailed},
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Scope != "//foo:broken" || r.Name != "//foo:broken" || r.Result != "failed" {
		t.Errorf("got %+v, want scope/name //foo:broken result failed", r)
	}
}
