// Package bep parses the Bazel Build Event Protocol (BEP) and extracts
// per-target test results. It supports both the binary, length-delimited
// stream (--build_event_binary_file) and the JSON stream
// (--build_event_json_file).
//
// It deliberately uses the prebuilt Bazel protobuf definitions from
// github.com/aspect-build/aspect-cli/bazel/buildeventstream rather than
// compiling protos in this repository.
package bep

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	bes "github.com/aspect-build/aspect-cli/bazel/buildeventstream"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/encoding/protojson"
)

// Status is the bktec-agnostic test status derived from a BEP TestStatus.
type Status string

const (
	StatusUnknown Status = "unknown"
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusFlaky   Status = "flaky"
	StatusSkipped Status = "skipped"
)

// TargetResult is the aggregated test result for a single Bazel target (label).
type TargetResult struct {
	// Label is the Bazel target label, e.g. "//foo:bar".
	Label string
	// Status is the aggregate status for the target. When a TestSummary event
	// is present it is authoritative; otherwise it is derived from the last
	// TestResult event.
	Status Status
	// XMLPaths is the deduplicated list of local JUnit XML file paths emitted
	// by the target's test actions (test.xml). Remote (non file://) outputs are
	// skipped because bktec cannot read them locally.
	XMLPaths []string
	// Cached is true when at least one test action for this target was served
	// from cache (local or remote).
	Cached   bool
	StartAt  float64
	Duration float64
}

// ParseFile reads a BEP file (binary or JSON) and returns the per-target test
// results in the order their labels were first seen.
func ParseFile(path string) ([]TargetResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read BEP file %s: %w", path, err)
	}

	events, err := parseEvents(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse BEP file %s: %w", path, err)
	}

	return aggregate(events), nil
}

// parseEvents decodes the BEP byte stream into BuildEvents. It detects the
// JSON format heuristically and falls back to the binary parser.
func parseEvents(data []byte) ([]*bes.BuildEvent, error) {
	if looksLikeJSON(data) {
		if events, err := parseJSON(bytes.NewReader(data)); err == nil {
			return events, nil
		}
		// Fall through to the binary parser if JSON decoding failed.
	}
	return parseBinary(bytes.NewReader(data))
}

func looksLikeJSON(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// parseBinary decodes a length-delimited stream of BuildEvent messages, as
// produced by `bazel ... --build_event_binary_file`.
func parseBinary(r io.Reader) ([]*bes.BuildEvent, error) {
	reader := bufio.NewReader(r)
	var events []*bes.BuildEvent
	for {
		event := &bes.BuildEvent{}
		err := protodelim.UnmarshalFrom(reader, event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// parseJSON decodes a stream of JSON-encoded BuildEvent messages, as produced
// by `bazel ... --build_event_json_file`. Bazel writes one JSON object per
// event, concatenated (and typically newline-delimited).
func parseJSON(r io.Reader) ([]*bes.BuildEvent, error) {
	dec := json.NewDecoder(r)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}

	var events []*bes.BuildEvent
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		event := &bes.BuildEvent{}
		if err := unmarshaler.Unmarshal(raw, event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

//nolint:staticcheck // SA1019: preserve timing from legacy BEP millisecond fields.
func aggregate(events []*bes.BuildEvent) []TargetResult {
	byLabel := map[string]*TargetResult{}
	order := []string{}

	get := func(label string) *TargetResult {
		t, ok := byLabel[label]
		if !ok {
			t = &TargetResult{Label: label, Status: StatusUnknown}
			byLabel[label] = t
			order = append(order, label)
		}
		return t
	}

	for _, event := range events {
		if tr := event.GetTestResult(); tr != nil {
			label := event.GetId().GetTestResult().GetLabel()
			if label == "" {
				continue
			}
			t := get(label)
			t.StartAt = float64(tr.GetTestAttemptStartMillisEpoch()) / 1000
			if start := tr.GetTestAttemptStart(); start != nil {
				t.StartAt = float64(start.GetSeconds()) + float64(start.GetNanos())/1e9
			}
			t.Duration = float64(tr.GetTestAttemptDurationMillis()) / 1000
			if duration := tr.GetTestAttemptDuration(); duration != nil {
				t.Duration = duration.AsDuration().Seconds()
			}
			for _, f := range tr.GetTestActionOutput() {
				if !strings.HasSuffix(f.GetName(), ".xml") {
					continue
				}
				if p := uriToPath(f.GetUri()); p != "" {
					t.XMLPaths = appendUnique(t.XMLPaths, p)
				}
			}
			if tr.GetCachedLocally() || tr.GetExecutionInfo().GetCachedRemotely() {
				t.Cached = true
			}
			// Provisional status; overwritten by TestSummary below if present.
			t.Status = mapStatus(tr.GetStatus())
		}

		if ts := event.GetTestSummary(); ts != nil {
			label := event.GetId().GetTestSummary().GetLabel()
			if label == "" {
				continue
			}
			// TestSummary.overall_status is authoritative and accounts for
			// flaky retries; it is emitted after the per-attempt TestResults.
			target := get(label)
			target.Status = mapStatus(ts.GetOverallStatus())
			if start := ts.GetFirstStartTime(); start != nil {
				target.StartAt = float64(start.GetSeconds()) + float64(start.GetNanos())/1e9
			} else if ts.GetFirstStartTimeMillis() != 0 {
				target.StartAt = float64(ts.GetFirstStartTimeMillis()) / 1000
			}
			if duration := ts.GetTotalRunDuration(); duration != nil {
				target.Duration = duration.AsDuration().Seconds()
			} else if ts.GetTotalRunDurationMillis() != 0 {
				target.Duration = float64(ts.GetTotalRunDurationMillis()) / 1000
			}
		}
	}

	results := make([]TargetResult, 0, len(order))
	for _, label := range order {
		results = append(results, *byLabel[label])
	}
	return results
}

func mapStatus(s bes.TestStatus) Status {
	switch s {
	case bes.TestStatus_PASSED:
		return StatusPassed
	case bes.TestStatus_FLAKY:
		return StatusFlaky
	case bes.TestStatus_FAILED,
		bes.TestStatus_TIMEOUT,
		bes.TestStatus_INCOMPLETE,
		bes.TestStatus_REMOTE_FAILURE,
		bes.TestStatus_FAILED_TO_BUILD,
		bes.TestStatus_TOOL_HALTED_BEFORE_TESTING:
		return StatusFailed
	default:
		return StatusUnknown
	}
}

// uriToPath converts a BEP file URI to a local filesystem path. Non-local URIs
// (e.g. bytestream:// for remotely cached outputs) return an empty string.
func uriToPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		return strings.TrimPrefix(uri, "file://")
	}
	return ""
}

func appendUnique(slice []string, value string) []string {
	for _, existing := range slice {
		if existing == value {
			return slice
		}
	}
	return append(slice, value)
}
