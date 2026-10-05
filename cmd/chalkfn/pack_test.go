package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/chalk-ai/chalk-go-function/internal/arrowjson"
)

func TestSourceFilesShipsOnlyTheImportGraph(t *testing.T) {
	target, err := resolveTarget("../../examples/add_one")
	if err != nil {
		t.Fatal(err)
	}
	if target.PkgDir != "examples/add_one" {
		t.Fatalf("PkgDir = %q", target.PkgDir)
	}
	files, err := sourceFiles(target, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "go.sum", "examples/add_one/main.go", "chalkfn/serve.go"} {
		if !slices.Contains(files, want) {
			t.Errorf("missing %s in %v", want, files)
		}
	}
	for _, f := range files {
		if strings.HasPrefix(f, "cmd/") || strings.HasPrefix(f, "examples/scalar") || strings.HasSuffix(f, "_test.go") {
			t.Errorf("unexpected file %s", f)
		}
	}

	a, err := tarGz(target.ModuleDir, files)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tarGz(target.ModuleDir, files)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("tarball is not deterministic")
	}
	if len(a) > maxInlineSourceBytes {
		t.Fatalf("example source is %d bytes, over the inline limit", len(a))
	}
}

func TestColumnsRoundTrip(t *testing.T) {
	schemaJSON := []byte(`{"columns":[{"name":"x","arrowType":{"int64":{}},"nullable":true},{"name":"s","arrowType":{"utf8":{}},"nullable":true}]}`)
	schema, err := arrowjsonSchema(schemaJSON)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := columnsToIPC(`{"x":[1,null,3],"s":["a","b",null]}`, schema)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ipcToColumns(payload)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"x": [1,null,3], "s": ["a","b",null]}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func arrowjsonSchema(raw []byte) (*arrow.Schema, error) { return arrowjson.ToArrowSchema(raw) }

func TestEmptyIPC(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: true}}, nil)
	payload, err := emptyIPC(schema)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ipcToColumns(payload)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"x": []}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
