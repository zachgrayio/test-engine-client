package api

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadTestResults(t *testing.T) {
	resultFile, err := os.CreateTemp("", "results-*.json")
	require.NoError(t, err)
	defer os.Remove(resultFile.Name())
	_, err = resultFile.WriteString(`{"examples":[]}`)
	require.NoError(t, err)
	resultFile.Close()

	var gotFormat, gotLocationPrefix string
	var gotToken string

	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		assert.Equal(t, "multipart/form-data", mediaType)

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			val, _ := io.ReadAll(part)
			switch part.FormName() {
			case "format":
				gotFormat = string(val)
			case "run_env[location_prefix]":
				gotLocationPrefix = string(val)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer svr.Close()

	client := NewClient(ClientConfig{UploadBaseURL: svr.URL})
	err = client.UploadTestResults(t.Context(), "my-token", resultFile.Name(), "rspec-json", "./")
	require.NoError(t, err)

	assert.Equal(t, "Token token=my-token", gotToken)
	assert.Equal(t, "rspec-json", gotFormat)
	assert.Equal(t, "./", gotLocationPrefix)
}

func TestUploadTestResults_IngestionResponse(t *testing.T) {
	resultFile := filepath.Join(t.TempDir(), "results.json")
	require.NoError(t, os.WriteFile(resultFile, []byte("[]"), 0o600))
	for _, testCase := range []struct {
		name      string
		body      string
		wantError string
	}{
		{name: "queued executions", body: `{"queued":2,"skipped":0,"errors":[]}`},
		{name: "ingestion error with HTTP success", body: `{"queued":0,"skipped":2,"errors":["history.duration is required"]}`, wantError: "history.duration is required"},
		{name: "no queued executions", body: `{"queued":0,"skipped":2,"errors":[]}`, wantError: "queued no test executions (skipped: 2)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(writer, testCase.body)
			}))
			defer server.Close()
			client := NewClient(ClientConfig{UploadBaseURL: server.URL})
			err := client.UploadTestResults(t.Context(), "upload-token", resultFile, "json", "")
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestUploadTestResults_ServerError(t *testing.T) {
	resultFile, err := os.CreateTemp("", "results-*.json")
	require.NoError(t, err)
	defer os.Remove(resultFile.Name())
	resultFile.Close()

	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer svr.Close()

	client := NewClient(ClientConfig{UploadBaseURL: svr.URL})
	err = client.UploadTestResults(t.Context(), "my-token", resultFile.Name(), "rspec-json", "")
	assert.ErrorContains(t, err, "upload failed with status 500")
}

func TestUploadTestResults_MissingFile(t *testing.T) {
	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer svr.Close()

	client := NewClient(ClientConfig{UploadBaseURL: svr.URL})
	err := client.UploadTestResults(t.Context(), "my-token", "/nonexistent/path/results.json", "rspec-json", "")
	assert.ErrorContains(t, err, "opening result file")
}

func TestBuildTestResultsMultipartBody(t *testing.T) {
	t.Setenv("BUILDKITE_BUILD_ID", "build-123")
	t.Setenv("BUILDKITE_BRANCH", "main")
	t.Setenv("BUILDKITE_COMMIT", "abc123")

	resultFile, err := os.CreateTemp("", "results-*.json")
	require.NoError(t, err)
	defer os.Remove(resultFile.Name())
	_, err = resultFile.WriteString(`{"examples":[{"id":"./spec/foo_spec.rb[1:1]"}]}`)
	require.NoError(t, err)
	resultFile.Close()

	buf, contentType, err := buildTestResultsMultipartBody(resultFile.Name(), "rspec-json", "my/prefix")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(contentType, "multipart/form-data"))

	mediaType, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	assert.Equal(t, "multipart/form-data", mediaType)

	fields := map[string]string{}
	mr := multipart.NewReader(buf, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		val, _ := io.ReadAll(part)
		if part.FormName() != "" {
			fields[part.FormName()] = string(val)
		}
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)

	assert.Equal(t, "rspec-json", fields["format"])
	assert.Equal(t, "buildkite", fields["run_env[CI]"])
	assert.Equal(t, "build-123", fields["run_env[key]"])
	assert.Equal(t, "main", fields["run_env[branch]"])
	assert.Equal(t, "abc123", fields["run_env[commit_sha]"])
	assert.Equal(t, "my/prefix", fields["run_env[location_prefix]"])
	assert.Equal(t, cwd, fields["run_env[cwd]"])
}

func TestBuildTestResultsMultipartBody_NoCwdOutsideBuildkite(t *testing.T) {
	t.Setenv("BUILDKITE_BUILD_ID", "")

	resultFile, err := os.CreateTemp("", "results-*.json")
	require.NoError(t, err)
	defer os.Remove(resultFile.Name())
	resultFile.Close()

	buf, contentType, err := buildTestResultsMultipartBody(resultFile.Name(), "rspec-json", "")
	require.NoError(t, err)

	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)

	fields := map[string]string{}
	mr := multipart.NewReader(buf, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		val, _ := io.ReadAll(part)
		if part.FormName() != "" {
			fields[part.FormName()] = string(val)
		}
	}

	assert.NotContains(t, fields, "run_env[cwd]")
}
