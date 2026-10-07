# chalk-go-function

Go SDK and CLI for deploying Apache Arrow functions to Chalk's external
function catalog. It is the Go counterpart of
[chalk-rs-function](https://github.com/chalk-ai/chalk-rs-function).

You write a function that takes and returns an Arrow record batch, register it,
and run `chalkfn deploy`.

## Quick start

```go
// examples/add_one/main.go
package main

import (
	"context"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/chalk-ai/chalk-go-function/chalkfn"
)

func init() {
	chalkfn.Register(chalkfn.Function{
		Name:   "add_one",
		Input:  chalkfn.Schema(chalkfn.Col("x", arrow.PrimitiveTypes.Int64)),
		Output: chalkfn.Schema(chalkfn.Col("result", arrow.PrimitiveTypes.Int64)),
		Fn:     addOne,
	})
}

func addOne(_ context.Context, mem memory.Allocator, in arrow.RecordBatch) (arrow.RecordBatch, error) {
	x := in.Column(0).(*array.Int64)
	b := array.NewInt64Builder(mem)
	defer b.Release()
	for i := 0; i < x.Len(); i++ {
		if x.IsNull(i) {
			b.AppendNull()
		} else {
			b.Append(x.Value(i) + 1)
		}
	}
	out := b.NewArray()
	defer out.Release()
	schema := chalkfn.Schema(chalkfn.Col("result", arrow.PrimitiveTypes.Int64))
	return array.NewRecordBatch(schema, []arrow.Array{out}, in.NumRows()), nil
}

func main() { chalkfn.Serve() }
```

For row-at-a-time functions over plain Go types, `Map1` through `Map9`
derive the schemas from the Go signature. A row with a null input produces a
null output without calling your function:

```go
chalkfn.Register(chalkfn.Map2("safe_divide", "numerator", "denominator", "result",
	func(n, d float64) (float64, error) {
		if d == 0 {
			return 0, errors.New("division by zero")
		}
		return n / d, nil
	}))
```

Supported Go types: `int8`–`int64`, `uint8`–`uint64`, `float32`, `float64`,
`bool`, `string` and `[]byte`.

## Try it locally

`chalkfn run` builds the package for your machine, serves it on a loopback
port and calls a function through the same gRPC contract Chalk uses:

```bash
go install github.com/chalk-ai/chalk-go-function/cmd/chalkfn@latest

chalkfn run ./examples/add_one --function add_one --input '{"x":[1,2,null,42]}'
# {"result": [2,3,null,43]}
```

Inputs and outputs are column-oriented JSON objects.

## Deploy and call

```bash
# Credentials: the token saved by `chalk login` is used by default.
# To use something else, set these:
export CHALK_API_SERVER=https://api.staging.chalk.dev
export CHALK_CLIENT_ID=...
export CHALK_CLIENT_SECRET=...
export CHALK_ENVIRONMENT_ID=...

chalkfn deploy ./examples/add_one --cpu 250m --memory 512Mi

chalkfn call --function add_one --input '{"x":[1,2,3,42]}'
# {"result": [2,3,4,43]}

chalkfn list
chalkfn delete --function add_one
```

`chalkfn deploy` deploys every function the package registers. Pass
`--function NAME` to deploy one. Other flags: `--min-replicas`,
`--max-replicas`, `--gpu`, `--env KEY=VALUE` (repeatable), `--builder-image`,
`--runtime-image`, `--cgo` and `--dry-run`. `--dry-run` prints the requests
without sending them.

## How it works

`chalkfn deploy`:

1. Builds the package for the host and runs `<binary> list-functions`, which
   prints every registered function with its Arrow schemas in
   `chalk.arrow.v1.Schema` JSON form.
2. Runs `go list -deps` for linux/amd64 and archives only `go.mod`, `go.sum`
   and the files of the module's own packages that the target imports. The
   remote build downloads third-party modules itself, so the archive is usually
   a few KB. The archive is deterministic: the same sources always produce the
   same bytes.
3. Sends a two-stage `ImageSpec` to
   `chalk.sandbox.v1.CustomImageService.GetOrBuildCustomImage`. The first
   stage compiles in `golang:<go.mod version>-bookworm`. The second stage
   copies the static binary and CA certificates into `debian:bookworm-slim`.
   The build cache is keyed on the spec content, so redeploying unchanged
   sources reuses the image.
4. Registers each function with
   `chalk.externalfunctioncatalog.v1.ExternalFunctionCatalogService.CreateExternalFunctionVersion`.
   The scaling group runs `/app/server`, which serves
   `chalk.runtime.v1.RemoteCallService` on port 6666 over gRPC, plus the
   standard gRPC health service. A function stays bound to the scaling group
   of its first version. A redeploy adds a version to that group. A new
   function gets a group named after the function.
5. Polls `ScalingGroupManagerService.GetScalingGroup` until a replica is ready.
   Then it calls the function with an empty batch until the API server can
   route to it. When `deploy` returns, `call` works.

A first deploy takes about a minute, most of it the remote `go build`.
Redeploying unchanged sources reuses the cached image and takes about 15
seconds.

`chalkfn call` goes through the API server's `CallExternalFunction` RPC, so it
needs no direct connection to the scaling group.

## Package layout

| Path | Purpose |
|------|---------|
| `chalkfn` | SDK: `Register`, `Function`, `Map1`–`Map9`, `Serve`, `Invoke` |
| `cmd/chalkfn` | CLI: `deploy`, `call`, `run`, `functions`, `list`, `delete` |
| `internal/arrowjson` | Arrow schema ⇄ `chalk.arrow.v1.Schema` JSON |
| `internal/gen` | Generated `chalk.runtime.v1` stubs (`go generate ./internal/gen`) |
| `internal/cmd/genmaps` | Generates `chalkfn/maps_gen.go` (`go generate ./chalkfn`) |
| `examples/add_one` | Batch-level example |
| `examples/scalar` | `Map1`/`Map2` example |
| `e2etests` | Integration test that deploys to a real environment (`-tags e2e`) |

## Supported Arrow types

The schema plumbing accepts primitives, `utf8`/`large_utf8`,
`binary`/`large_binary`, `fixed_size_binary`, dates, times, timestamps,
durations, decimals, lists, large lists, fixed-size lists, structs and maps.
`chalkfn call` and `chalkfn run` accept anything `array.RecordFromJSON` can
parse.

## Limitations

- Chalk's image service accepts at most 32 KiB of inline source per image. The
  archive includes only the target's own import graph, but a large module can
  still exceed the limit. If it does, move code the function does not import
  out of its package graph, or split the code into a published module.
- `replace` directives that point at local directories cannot be built
  remotely, and the CLI rejects them. Private modules need to be fetchable
  from the remote build without credentials.
- Images are built for linux/amd64.

## Continuous integration

Buildkite runs two pipelines from `.buildkite/`. The
`.github/workflows/sync-buildkite.yml` workflow syncs them to Buildkite with
[buildkite-sync-action](https://github.com/chalk-ai/buildkite-sync-action)
whenever `.buildkite/` changes on `main`.

| Pipeline | Runs |
|----------|------|
| `chalk-go-function-unit-tests` | gofmt, `go vet`, generated-code and `go mod tidy` checks, `go test -race ./...` |
| `chalk-go-function-integration-tests` | `e2etests` against staging and meta-ci |

The integration test deploys `e2etests/fixture` with the CLI, calls each
function through the API server, redeploys one function and checks that the
redeploy used the cached image and created version 2, then deletes everything
it deployed. Function names end in a random suffix, so concurrent runs do not
collide. Run it yourself with Chalk credentials in the environment:

```bash
go test -tags e2e -v -timeout 40m ./e2etests
```

CI credentials are in `.env.enc` (staging) and `.env.meta-ci.enc` (meta-ci),
encrypted with sops and the `chalk-ci-testing` KMS key in `.sops.yaml`.

## Testing your function

Use `chalkfn.Invoke` to run a registered function over an Arrow IPC payload in
a unit test, without starting a server.
