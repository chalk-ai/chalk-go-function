// Command fixture serves the functions the e2e tests deploy. Every name ends
// in $CHALKFN_E2E_SUFFIX so concurrent CI runs never share a function or a
// scaling group. The deployed container receives the same variable through
// `chalkfn deploy --env`, so it registers the same names it is called by.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/chalk-ai/chalk-go-function/chalkfn"
)

func init() {
	suffix := os.Getenv("CHALKFN_E2E_SUFFIX")
	if suffix == "" {
		suffix = "local"
	}

	chalkfn.Register(chalkfn.Function{
		Name:   "e2e_add_one_" + suffix,
		Input:  chalkfn.Schema(chalkfn.Col("x", arrow.PrimitiveTypes.Int64)),
		Output: chalkfn.Schema(chalkfn.Col("result", arrow.PrimitiveTypes.Int64)),
		Fn:     addOne,
	})

	chalkfn.Register(chalkfn.Map2("e2e_divide_"+suffix, "numerator", "denominator", "result",
		func(n, d float64) (float64, error) {
			if d == 0 {
				return 0, errors.New("division by zero")
			}
			return n / d, nil
		}))

	chalkfn.Register(chalkfn.Map4("e2e_mixed_"+suffix, "n", "s", "b", "f", "result",
		func(n int32, s string, b bool, f float64) (string, error) {
			return fmt.Sprintf("%d|%s|%t|%.2f", n, s, b, f), nil
		}))
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
	return array.NewRecordBatch(chalkfn.Schema(chalkfn.Col("result", arrow.PrimitiveTypes.Int64)), []arrow.Array{out}, in.NumRows()), nil
}

func main() { chalkfn.Serve() }
