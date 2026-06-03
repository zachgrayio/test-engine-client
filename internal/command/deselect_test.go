package command

import (
	"reflect"
	"testing"

	"github.com/buildkite/test-engine-client/v2/internal/plan"
)

func TestDeselectSkippedTargets(t *testing.T) {
	task := &plan.Task{
		Tests: []plan.TestCase{
			{Path: "//foo:bar"},
			{Path: "//baz:qux"},
			{Path: "//keep:me"},
		},
	}
	skipped := []plan.TestCase{
		{Scope: "//foo:bar", Name: "test_one"},
		{Scope: "//baz:qux", Name: "test_two"},
		// scope with no matching target should be ignored
		{Scope: "//not:here", Name: "test_three"},
	}

	deselected := deselectSkippedTargets(task, skipped)

	wantDeselected := []string{"//foo:bar", "//baz:qux"}
	if !reflect.DeepEqual(deselected, wantDeselected) {
		t.Errorf("deselected = %v, want %v", deselected, wantDeselected)
	}

	if len(task.Tests) != 1 || task.Tests[0].Path != "//keep:me" {
		t.Errorf("remaining tests = %v, want only //keep:me", task.Tests)
	}
}

func TestDeselectSkippedTargets_NoSkips(t *testing.T) {
	task := &plan.Task{
		Tests: []plan.TestCase{{Path: "//foo:bar"}},
	}
	deselected := deselectSkippedTargets(task, nil)
	if deselected != nil {
		t.Errorf("expected no deselected targets, got %v", deselected)
	}
	if len(task.Tests) != 1 {
		t.Errorf("tests should be unchanged, got %v", task.Tests)
	}
}
