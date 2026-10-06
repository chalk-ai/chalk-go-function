// Package chalkfn is the SDK for writing Chalk external functions in Go.
//
// A function receives an Arrow record batch and returns one. Register it from
// an init function (or at the top of main) and call [Serve] from main:
//
//	func init() {
//		chalkfn.Register(chalkfn.Function{
//			Name:   "add_one",
//			Input:  chalkfn.Schema(chalkfn.Col("x", arrow.PrimitiveTypes.Int64)),
//			Output: chalkfn.Schema(chalkfn.Col("result", arrow.PrimitiveTypes.Int64)),
//			Fn:     addOne,
//		})
//	}
//
//	func main() { chalkfn.Serve() }
//
// For row-at-a-time scalar functions over primitive Go types, [Map1], [Map2]
// and [Map3] derive the schemas and the Arrow plumbing from the Go signature.
//
// Deploy the binary with the chalkfn CLI (github.com/chalk-ai/chalk-go-function/cmd/chalkfn).
package chalkfn

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/chalk-ai/chalk-go-function/internal/arrowjson"
)

// BatchFunc transforms one input record batch into one output record batch.
//
// The input's columns are positional and follow the function's Input schema.
// The returned record must have the same number of rows as the input and match
// the Output schema; the runtime releases it after serializing it. Allocate
// output arrays from mem.
type BatchFunc func(ctx context.Context, mem memory.Allocator, in arrow.RecordBatch) (arrow.RecordBatch, error)

// Function is a function registered for remote invocation.
type Function struct {
	// Name is the catalog name callers invoke the function by.
	Name string
	// Input is the schema of the record batches the function accepts.
	Input *arrow.Schema
	// Output is the schema of the record batches the function returns.
	Output *arrow.Schema
	// Fn computes the output batch.
	Fn BatchFunc
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Function{}
)

// The catalog derives Kubernetes object names from function names, so keep
// them to identifier-like strings.
var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-]*$`)

// Register adds f to the set of functions this binary serves. It panics if f
// is malformed or its name is already registered, so mistakes surface at
// startup rather than on the first call.
func Register(f Function) {
	if err := f.validate(); err != nil {
		panic(fmt.Sprintf("chalkfn.Register: %v", err))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[f.Name]; dup {
		panic(fmt.Sprintf("chalkfn.Register: function %q registered twice", f.Name))
	}
	registry[f.Name] = f
}

func (f Function) validate() error {
	if !validName.MatchString(f.Name) {
		return fmt.Errorf("invalid function name %q (want letters, digits, '_' or '-')", f.Name)
	}
	if f.Input == nil || f.Output == nil {
		return fmt.Errorf("function %q: Input and Output schemas are required", f.Name)
	}
	if f.Output.NumFields() == 0 {
		return fmt.Errorf("function %q: Output schema has no columns", f.Name)
	}
	if f.Fn == nil {
		return fmt.Errorf("function %q: Fn is nil", f.Name)
	}
	for _, s := range []*arrow.Schema{f.Input, f.Output} {
		for _, field := range s.Fields() {
			if _, err := arrowjson.TypeJSON(field.Type); err != nil {
				return fmt.Errorf("function %q column %q: %w", f.Name, field.Name, err)
			}
		}
	}
	return nil
}

// Functions returns every registered function, sorted by name.
func Functions() []Function {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Function, 0, len(registry))
	for _, f := range registry {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the registered function with the given name.
func Lookup(name string) (Function, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// Schema builds an Arrow schema from fields.
func Schema(fields ...arrow.Field) *arrow.Schema {
	return arrow.NewSchema(fields, nil)
}

// Col declares a nullable column.
func Col(name string, t arrow.DataType) arrow.Field {
	return arrow.Field{Name: name, Type: t, Nullable: true}
}

// NotNullCol declares a non-nullable column.
func NotNullCol(name string, t arrow.DataType) arrow.Field {
	return arrow.Field{Name: name, Type: t, Nullable: false}
}
