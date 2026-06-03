package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/buildkite/test-engine-client/v2/internal/api"
	"github.com/buildkite/test-engine-client/v2/internal/bep"
	"github.com/buildkite/test-engine-client/v2/internal/config"
	"github.com/buildkite/test-engine-client/v2/internal/junit"
)

// testEngineUploadResult is a single entry in the Test Engine importing-json
// upload format. scope is set to the Bazel target label so the uploaded
// identities match what the bazel runner records locally (scope/name), keeping
// mute/quarantine matching consistent.
type testEngineUploadResult struct {
	Scope   string            `json:"scope,omitempty"`
	Name    string            `json:"name"`
	Result  string            `json:"result"`
	History testEngineHistory `json:"history"`
}

type testEngineHistory struct {
	Section string `json:"section"`
}

// ProcessBEP parses a Bazel BEP file, derives per-case Test Engine results
// (scope = target label, name = JUnit testcase) and uploads them to the Test
// Engine analytics API. It does NOT invoke bazel; it only ingests an
// already-produced BEP file. This is the analytics-only path used by the
// test-collector plugin's bazel-bep mode.
func ProcessBEP(ctx context.Context, cfg *config.Config) error {
	if cfg.BepPath == "" {
		return fmt.Errorf("a BEP file path is required (set BUILDKITE_TEST_ENGINE_BEP_PATH or --bep-path)")
	}
	if cfg.UploadToken == "" {
		return fmt.Errorf("an upload token is required (set BUILDKITE_ANALYTICS_TOKEN)")
	}

	targets, err := bep.ParseFile(cfg.BepPath)
	if err != nil {
		return fmt.Errorf("failed to parse BEP file: %w", err)
	}

	results := bepTargetsToUploadResults(targets)
	if len(results) == 0 {
		fmt.Println("Buildkite Test Engine Client: No test results found in BEP file")
		return nil
	}

	tmp, err := os.CreateTemp("", "bktec-bep-results-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp results file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := json.NewEncoder(tmp).Encode(results); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to encode results: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp results file: %w", err)
	}

	apiClient := api.NewClient(api.ClientConfig{
		UploadBaseURL: cfg.UploadBaseURL,
	})

	fmt.Printf("Buildkite Test Engine Client: Uploading %d Bazel test results to Test Engine\n", len(results))
	if err := apiClient.UploadTestResults(ctx, cfg.UploadToken, tmp.Name(), "json", cfg.LocationPrefix); err != nil {
		return fmt.Errorf("failed to upload test results: %w", err)
	}

	return nil
}

func bepTargetsToUploadResults(targets []bep.TargetResult) []testEngineUploadResult {
	var results []testEngineUploadResult

	for _, target := range targets {
		recorded := false
		for _, xmlPath := range target.XMLPaths {
			cases, err := junit.Parse(xmlPath)
			if err != nil {
				fmt.Printf("Buildkite Test Engine Client: Failed to parse JUnit XML %s for %s: %v\n", xmlPath, target.Label, err)
				continue
			}
			for _, tc := range cases {
				results = append(results, testEngineUploadResult{
					Scope:   target.Label,
					Name:    tc.Name,
					Result:  string(tc.Result),
					History: testEngineHistory{Section: "test"},
				})
				recorded = true
			}
		}

		if !recorded {
			// No per-case XML; record a single target-level result so the
			// target still appears (and matches target-level mutes).
			results = append(results, testEngineUploadResult{
				Scope:   target.Label,
				Name:    target.Label,
				Result:  bepStatusToResultString(target.Status),
				History: testEngineHistory{Section: "test"},
			})
		}
	}

	return results
}

func bepStatusToResultString(s bep.Status) string {
	switch s {
	case bep.StatusPassed, bep.StatusFlaky:
		return "passed"
	case bep.StatusFailed:
		return "failed"
	case bep.StatusSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}
