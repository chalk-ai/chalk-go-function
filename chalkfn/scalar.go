package chalkfn

import (
	"context"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Scalar is a Go type that maps one-to-one onto an Arrow column type.
type Scalar interface {
	int8 | int16 | int32 | int64 | uint8 | uint16 | uint32 | uint64 |
		float32 | float64 | bool | string | []byte
}

// ArrowType returns the Arrow type a Scalar Go type maps to.
func ArrowType[T Scalar]() arrow.DataType {
	var zero T
	switch any(zero).(type) {
	case int8:
		return arrow.PrimitiveTypes.Int8
	case int16:
		return arrow.PrimitiveTypes.Int16
	case int32:
		return arrow.PrimitiveTypes.Int32
	case int64:
		return arrow.PrimitiveTypes.Int64
	case uint8:
		return arrow.PrimitiveTypes.Uint8
	case uint16:
		return arrow.PrimitiveTypes.Uint16
	case uint32:
		return arrow.PrimitiveTypes.Uint32
	case uint64:
		return arrow.PrimitiveTypes.Uint64
	case float32:
		return arrow.PrimitiveTypes.Float32
	case float64:
		return arrow.PrimitiveTypes.Float64
	case bool:
		return arrow.FixedWidthTypes.Boolean
	case string:
		return arrow.BinaryTypes.String
	case []byte:
		return arrow.BinaryTypes.Binary
	}
	panic(fmt.Sprintf("chalkfn: no arrow type for %T", zero))
}

// Map1 builds a Function that applies fn to each row of a one-column input.
// Rows where the input is null produce a null output without calling fn, and
// an error from fn fails the whole batch.
//
//	chalkfn.Register(chalkfn.Map1("add_one", "x", "result",
//		func(x int64) (int64, error) { return x + 1, nil }))
func Map1[A, R Scalar](name, a, result string, fn func(A) (R, error)) Function {
	return Function{
		Name:   name,
		Input:  Schema(Col(a, ArrowType[A]())),
		Output: Schema(Col(result, ArrowType[R]())),
		Fn: rowwise[R](func(cols []arrow.Array, out func(R), i int) error {
			r, err := fn(valueAt[A](cols[0], i))
			if err == nil {
				out(r)
			}
			return err
		}),
	}
}

// Map2 is [Map1] for two input columns.
func Map2[A, B, R Scalar](name, a, b, result string, fn func(A, B) (R, error)) Function {
	return Function{
		Name:   name,
		Input:  Schema(Col(a, ArrowType[A]()), Col(b, ArrowType[B]())),
		Output: Schema(Col(result, ArrowType[R]())),
		Fn: rowwise[R](func(cols []arrow.Array, out func(R), i int) error {
			r, err := fn(valueAt[A](cols[0], i), valueAt[B](cols[1], i))
			if err == nil {
				out(r)
			}
			return err
		}),
	}
}

// Map3 is [Map1] for three input columns.
func Map3[A, B, C, R Scalar](name, a, b, c, result string, fn func(A, B, C) (R, error)) Function {
	return Function{
		Name:   name,
		Input:  Schema(Col(a, ArrowType[A]()), Col(b, ArrowType[B]()), Col(c, ArrowType[C]())),
		Output: Schema(Col(result, ArrowType[R]())),
		Fn: rowwise[R](func(cols []arrow.Array, out func(R), i int) error {
			r, err := fn(valueAt[A](cols[0], i), valueAt[B](cols[1], i), valueAt[C](cols[2], i))
			if err == nil {
				out(r)
			}
			return err
		}),
	}
}

// rowwise lifts a per-row callback into a BatchFunc producing one column of
// type R. A row with any null input yields a null output.
func rowwise[R Scalar](row func(cols []arrow.Array, out func(R), i int) error) BatchFunc {
	return func(ctx context.Context, mem memory.Allocator, in arrow.RecordBatch) (arrow.RecordBatch, error) {
		cols := in.Columns()
		b := array.NewBuilder(mem, ArrowType[R]())
		defer b.Release()
		b.Reserve(int(in.NumRows()))
		appendR := appender[R](b)
		for i := 0; i < int(in.NumRows()); i++ {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if anyNull(cols, i) {
				b.AppendNull()
				continue
			}
			if err := row(cols, appendR, i); err != nil {
				return nil, fmt.Errorf("row %d: %w", i, err)
			}
		}
		col := b.NewArray()
		defer col.Release()
		schema := Schema(Col("result", col.DataType()))
		return array.NewRecordBatch(schema, []arrow.Array{col}, in.NumRows()), nil
	}
}

func anyNull(cols []arrow.Array, i int) bool {
	for _, c := range cols {
		if c.IsNull(i) {
			return true
		}
	}
	return false
}

// valueAt reads row i of arr as T. The runtime has already checked the column
// count; a column whose Arrow type does not match T panics, which Invoke
// reports as an error for the batch.
func valueAt[T Scalar](arr arrow.Array, i int) T {
	var v any
	switch a := arr.(type) {
	case *array.Int8:
		v = a.Value(i)
	case *array.Int16:
		v = a.Value(i)
	case *array.Int32:
		v = a.Value(i)
	case *array.Int64:
		v = a.Value(i)
	case *array.Uint8:
		v = a.Value(i)
	case *array.Uint16:
		v = a.Value(i)
	case *array.Uint32:
		v = a.Value(i)
	case *array.Uint64:
		v = a.Value(i)
	case *array.Float32:
		v = a.Value(i)
	case *array.Float64:
		v = a.Value(i)
	case *array.Boolean:
		v = a.Value(i)
	case *array.String:
		v = a.Value(i)
	case *array.LargeString:
		v = a.Value(i)
	case *array.Binary:
		v = a.Value(i)
	case *array.LargeBinary:
		v = a.Value(i)
	}
	t, ok := v.(T)
	if !ok {
		var zero T
		panic(fmt.Sprintf("column of type %s cannot be read as %T", arr.DataType(), zero))
	}
	return t
}

func appender[T Scalar](b array.Builder) func(T) {
	switch b := b.(type) {
	case *array.Int8Builder:
		return func(v T) { b.Append(any(v).(int8)) }
	case *array.Int16Builder:
		return func(v T) { b.Append(any(v).(int16)) }
	case *array.Int32Builder:
		return func(v T) { b.Append(any(v).(int32)) }
	case *array.Int64Builder:
		return func(v T) { b.Append(any(v).(int64)) }
	case *array.Uint8Builder:
		return func(v T) { b.Append(any(v).(uint8)) }
	case *array.Uint16Builder:
		return func(v T) { b.Append(any(v).(uint16)) }
	case *array.Uint32Builder:
		return func(v T) { b.Append(any(v).(uint32)) }
	case *array.Uint64Builder:
		return func(v T) { b.Append(any(v).(uint64)) }
	case *array.Float32Builder:
		return func(v T) { b.Append(any(v).(float32)) }
	case *array.Float64Builder:
		return func(v T) { b.Append(any(v).(float64)) }
	case *array.BooleanBuilder:
		return func(v T) { b.Append(any(v).(bool)) }
	case *array.StringBuilder:
		return func(v T) { b.Append(any(v).(string)) }
	case *array.BinaryBuilder:
		return func(v T) { b.Append(any(v).([]byte)) }
	}
	panic(fmt.Sprintf("chalkfn: no appender for builder %T", b))
}

// arrowRecord re-labels rec's columns with schema.
func arrowRecord(schema *arrow.Schema, rec arrow.RecordBatch) arrow.RecordBatch {
	return array.NewRecordBatch(schema, rec.Columns(), rec.NumRows())
}
