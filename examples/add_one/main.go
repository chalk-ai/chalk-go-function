// Command add_one serves a Chalk external function that adds 1 to every
// element of an int64 column, written against the batch-level API.
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
	b.Reserve(x.Len())
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

func main() {
	chalkfn.Serve()
}
