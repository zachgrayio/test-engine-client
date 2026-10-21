package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	bes "github.com/aspect-build/aspect-cli/bazel/buildeventstream"
	"github.com/buildkite/test-engine-client/v2/internal/api"
	"github.com/buildkite/test-engine-client/v2/internal/bep"
	"github.com/buildkite/test-engine-client/v2/internal/config"
	"github.com/buildkite/test-engine-client/v2/internal/runner"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestUploadResults_Bazel(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		enabled bool
		binary  bool
	}{
		{name: "JSON disabled"},
		{name: "JSON enabled", enabled: true},
		{name: "binary disabled", binary: true},
		{name: "binary enabled", enabled: true, binary: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bepPath := filepath.Join(t.TempDir(), "bep.json")
			data := `{"id":{"testSummary":{"label":"//:passing"}},"testSummary":{"overallStatus":"PASSED","firstStartTimeMillis":"1700000000125","totalRunDurationMillis":"500"}}
{"id":{"testSummary":{"label":"//:failing"}},"testSummary":{"overallStatus":"FAILED","firstStartTimeMillis":"1700000000125","totalRunDurationMillis":"500"}}
`
			bepData := []byte(data)
			if testCase.binary {
				bepPath = filepath.Join(t.TempDir(), "bep.pb")
				var buffer bytes.Buffer
				for _, target := range []struct {
					label  string
					status bes.TestStatus
				}{
					{label: "//:passing", status: bes.TestStatus_PASSED},
					{label: "//:failing", status: bes.TestStatus_FAILED},
				} {
					event := &bes.BuildEvent{
						Id: &bes.BuildEventId{Id: &bes.BuildEventId_TestSummary{TestSummary: &bes.BuildEventId_TestSummaryId{Label: target.label}}},
						Payload: &bes.BuildEvent_TestSummary{TestSummary: &bes.TestSummary{
							OverallStatus:    target.status,
							FirstStartTime:   &timestamppb.Timestamp{Seconds: 1700000000, Nanos: 125000000},
							TotalRunDuration: &durationpb.Duration{Nanos: 500000000},
						}},
					}
					if _, err := protodelim.MarshalTo(&buffer, event); err != nil {
						t.Fatal(err)
					}
				}
				bepData = buffer.Bytes()
			}
			if err := os.WriteFile(bepPath, bepData, 0o600); err != nil {
				t.Fatal(err)
			}
			var uploaded []testEngineUploadResult
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/uploads" {
					t.Errorf("unexpected upload: %s %s", request.Method, request.URL.Path)
				}
				if got := request.Header.Get("Authorization"); got != "Token token=upload-token" {
					t.Errorf("authorization = %q", got)
				}
				if err := request.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				defer request.MultipartForm.RemoveAll()
				if got := request.FormValue("format"); got != "json" {
					t.Errorf("format = %q, want json", got)
				}
				file, _, err := request.FormFile("data")
				if err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				defer file.Close()
				payload, err := io.ReadAll(file)
				if err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				var wireResults []map[string]json.RawMessage
				if err := json.Unmarshal(payload, &wireResults); err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				for _, result := range wireResults {
					var history map[string]json.RawMessage
					if err := json.Unmarshal(result["history"], &history); err != nil {
						t.Error(err)
						continue
					}
					for _, field := range []string{"start_at", "duration"} {
						var value float64
						data, ok := history[field]
						if !ok || bytes.Equal(data, []byte("null")) {
							t.Errorf("missing required numeric history.%s in %s", field, payload)
						} else if err := json.Unmarshal(data, &value); err != nil {
							t.Errorf("history.%s is not numeric: %s", field, data)
						}
					}
				}
				if err := json.Unmarshal(payload, &uploaded); err != nil {
					t.Error(err)
				}
				writer.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(writer, `{}`)
			}))
			defer server.Close()
			cfg := &config.Config{UploadResults: testCase.enabled, UploadToken: "upload-token"}
			client := api.NewClient(api.ClientConfig{UploadBaseURL: server.URL})
			testRunner := runner.NewBazel(runner.RunnerConfig{ResultPath: bepPath})
			uploadResults(context.Background(), client, cfg, testRunner)
			if !testCase.enabled {
				if len(uploaded) != 0 {
					t.Fatal("uploaded results without opt-in")
				}
				return
			}
			if len(uploaded) != 2 {
				t.Fatalf("uploaded %d results, want 2", len(uploaded))
			}
			statuses := make(map[string]string)
			for _, result := range uploaded {
				statuses[result.Scope] = result.Result
				if result.History.StartAt != 1700000000.125 || result.History.Duration != 0.5 {
					t.Errorf("BEP timing not preserved in upload: %+v", result.History)
				}
			}
			if statuses["//:passing"] != "passed" || statuses["//:failing"] != "failed" {
				t.Errorf("unexpected results: %v", statuses)
			}
		})
	}
}

func TestBepTargetsToUploadResults_WithXML(t *testing.T) {
	xml := `<?xml version="1.0"?>
<testsuites>
  <testsuite name="//foo:bar">
	<testcase classname="foo.Bar" name="test_one" time="0.25"/>
	<testcase classname="foo.Bar" name="test_two" time="0.75"><failure message="boom"/></testcase>
  </testsuite>
</testsuites>`
	xmlPath := filepath.Join(t.TempDir(), "test.xml")
	if err := os.WriteFile(xmlPath, []byte(xml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	results := bepTargetsToUploadResults([]bep.TargetResult{
		{Label: "//foo:bar", Status: bep.StatusFailed, XMLPaths: []string{xmlPath}, StartAt: 123.5},
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Scope != "//foo:bar" {
			t.Errorf("scope = %q, want //foo:bar", r.Scope)
		}
		if r.History.StartAt != 123.5 {
			t.Errorf("history.start_at = %v, want 123.5", r.History.StartAt)
		}
		wantDuration := map[string]float64{"test_one": 0.25, "test_two": 0.75}[r.Name]
		if r.History.Duration != wantDuration {
			t.Errorf("history.duration = %v, want %v", r.History.Duration, wantDuration)
		}
	}
}

func TestBepTargetsToUploadResults_NoXML(t *testing.T) {
	results := bepTargetsToUploadResults([]bep.TargetResult{
		{Label: "//foo:broken", Status: bep.StatusFailed, StartAt: 100.5, Duration: 1.25},
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Scope != "//foo:broken" || r.Name != "//foo:broken" || r.Result != "failed" {
		t.Errorf("got %+v, want scope/name //foo:broken result failed", r)
	}
	if r.History.StartAt != 100.5 || r.History.Duration != 1.25 {
		t.Errorf("BEP timing not preserved: %+v", r.History)
	}
}
