# Using bktec with Bazel

The `bazel` test runner makes `bktec` the orchestrator for Bazel test runs: it
discovers test targets, splits them across parallel nodes, invokes `bazel test`,
ingests the [Build Event Protocol (BEP)](https://bazel.build/remote/bep) that
Bazel emits, and maps the results into Buildkite Test Engine identities. This
lets you use Test Engine features (test splitting, muting/quarantine, and
automatic retries) with a Bazel monorepo.

```sh
# Tell bktec to use the Bazel test runner integration
export BUILDKITE_TEST_ENGINE_TEST_RUNNER=bazel

export BUILDKITE_TEST_ENGINE_SUITE_SLUG=your-suite-slug
export BUILDKITE_TEST_ENGINE_API_ACCESS_TOKEN=your-token

# bktec invokes `bazel test` for you
bktec run
```

## How it works

1. `bktec` discovers test targets (see [Target discovery](#target-discovery)).
2. The Test Engine API splits the targets across `BUILDKITE_PARALLEL_JOB_COUNT`
   nodes.
3. For this node, `bktec` invokes:

   ```sh
   bazel test <bazel-flags> --build_event_binary_file=<bep-path> //label-a //label-b ...
   ```

4. `bktec` parses the BEP file, reads the JUnit `test.xml` outputs that BEP
   references for each target, and records one Test Engine result per
   `<testcase>`.

### Test identity

Each test result is recorded with:

- `scope` = the Bazel target label (e.g. `//foo:bar`)
- `name` = the JUnit `<testcase>` name from the target's `test.xml`

Because `scope` is the target label, Test Engine lifecycle actions group
naturally by target. Targets that fail to build (and therefore emit no
`test.xml`) are recorded as a single target-level result with `scope == name ==
label`.

## Target discovery

By default, `bktec` discovers test targets using:

```sh
bazel query 'tests(//...)' --output=label
```

You can change the query with `BUILDKITE_TEST_ENGINE_BAZEL_QUERY`:

```sh
export BUILDKITE_TEST_ENGINE_BAZEL_QUERY='tests(//services/...)'
```

Alternatively, you can bypass discovery entirely and provide a precomputed list
of target labels (one per line) via the `--files` flag / `BUILDKITE_TEST_ENGINE_FILES`
environment variable. This is useful when the set of targets is computed
elsewhere (e.g. from a PR diff):

```sh
bazel query 'tests(//...)' --output=label > targets.txt
BUILDKITE_TEST_ENGINE_FILES=targets.txt bktec run
```

## Bazel flags

Pass extra flags to `bazel test` with `BUILDKITE_TEST_ENGINE_BAZEL_FLAGS` (a
shell-quoted string):

```sh
export BUILDKITE_TEST_ENGINE_BAZEL_FLAGS="--config=ci --build_tests_only --test_tag_filters=-manual"
```

`bktec` always appends `--build_event_binary_file=<path>` itself so it can read
the results; you don't need to add it.

## BEP file location

By default `bktec` writes the BEP to a temporary file. Set
`BUILDKITE_TEST_ENGINE_BEP_PATH` to control where it is written, for example so
another tool (such as a BEP annotation plugin) can read the same file:

```sh
export BUILDKITE_TEST_ENGINE_BEP_PATH="$BUILDKITE_BUILD_PATH/bep/$BUILDKITE_JOB_ID/bep-0.pb"
```

## Test state management: mute vs. skip

`bktec` honors the Buildkite Test Engine [test state and quarantine](https://buildkite.com/docs/test-engine/test-suites/test-state-and-quarantine)
lifecycle. Because Bazel can only act at the target level when `bktec` invokes
it, the two states map as follows:

- **Skip** (quarantine): any target that contains a skipped test is dropped from
  the `bazel test` invocation entirely. This is the best `bktec` can do when it
  drives Bazel, since Bazel cannot skip an individual test case inside a target.
- **Mute**: the target still runs. If the only failing test cases are muted, the
  build is not failed (the non-zero Bazel exit code is suppressed).

> [!NOTE]
> Bazel signals test failures with exit code `3`. `bktec` treats this as a test
> failure (eligible for muted-failure suppression); build errors (exit `1`) and
> "no tests found" (exit `4`) are always propagated.

## Automatically retry failed tests

Set `BUILDKITE_TEST_ENGINE_RETRY_COUNT` to retry failed targets:

```sh
export BUILDKITE_TEST_ENGINE_RETRY_COUNT=1
```

> [!TIP]
> Bazel has its own per-test retry/flaky handling (`--flaky_test_attempts`,
> `flaky = True`). Prefer Bazel's granular retries where possible; `bktec`'s
> retry re-invokes `bazel test` for the failed targets only.

## Uploading results to Test Engine

The `bazel` runner produces many `test.xml` files (one per target), so the
single-file `--upload-results` path is not used. Upload results with the
[test-collector plugin](https://github.com/buildkite-plugins/test-collector-buildkite-plugin)
pointed at Bazel's `bazel-testlogs`, or at the BEP file.

## Full Buildkite pipeline example

```yaml
- name: "Bazel test :bazel:"
  commands:
    - bktec run
  env:
    BUILDKITE_TEST_ENGINE_SUITE_SLUG: your-suite-slug
    BUILDKITE_TEST_ENGINE_API_ACCESS_TOKEN: your-api-token # For state management
    BUILDKITE_TEST_ENGINE_TEST_RUNNER: bazel
    BUILDKITE_TEST_ENGINE_BAZEL_FLAGS: "--config=ci --build_tests_only"
    BUILDKITE_TEST_ENGINE_BEP_PATH: "$BUILDKITE_BUILD_PATH/bep/$BUILDKITE_JOB_ID/bep-0.pb"
    BUILDKITE_TEST_ENGINE_RETRY_COUNT: 1
  parallelism: 4 # This activates test splitting across targets
```
