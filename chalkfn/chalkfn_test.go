package chalkfn

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func ipcFromJSON(t *testing.T, mem memory.Allocator, schema *arrow.Schema, rows string) []byte {
	t.Helper()
	rec, _, err := array.RecordFromJSON(mem, schema, strings.NewReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema), ipc.WithAllocator(mem))
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jsonFromIPC(t *testing.T, payload []byte) string {
	t.Helper()
	r, err := ipc.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	var out bytes.Buffer
	for r.Next() {
		if err := array.RecordToJSON(r.RecordBatch(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return strings.TrimSpace(out.String())
}

func TestInvokeMap2(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.NewGoAllocator())
	defer mem.AssertSize(t, 0)

	f := Map2("concat", "a", "n", "out", func(a string, n int64) (string, error) {
		return strings.Repeat(a, int(n)), nil
	})
	in := ipcFromJSON(t, mem, f.Input, `[{"a":"ab","n":2},{"a":null,"n":1},{"a":"x","n":3}]`)
	out, err := Invoke(context.Background(), mem, f, in)
	if err != nil {
		t.Fatal(err)
	}
	got := jsonFromIPC(t, out)
	want := "{\"out\":\"abab\"}\n{\"out\":null}\n{\"out\":\"xxx\"}"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestInvokeReportsFunctionErrors(t *testing.T) {
	mem := memory.NewGoAllocator()
	f := Map1("fail", "x", "y", func(x int64) (int64, error) {
		if x < 0 {
			return 0, errBoom
		}
		return x, nil
	})
	in := ipcFromJSON(t, mem, f.Input, `[{"x":1},{"x":-1}]`)
	_, err := Invoke(context.Background(), mem, f, in)
	if err == nil || !strings.Contains(err.Error(), "row 1: boom") {
		t.Fatalf("got %v, want row 1 error", err)
	}
}

func TestInvokeRejectsWrongOutputType(t *testing.T) {
	mem := memory.NewGoAllocator()
	f := Map1("bad", "x", "y", func(x int64) (int64, error) { return x, nil })
	f.Output = Schema(Col("y", arrow.BinaryTypes.String))
	in := ipcFromJSON(t, mem, f.Input, `[{"x":1}]`)
	_, err := Invoke(context.Background(), mem, f, in)
	if err == nil || !strings.Contains(err.Error(), "has type int64, declared utf8") {
		t.Fatalf("got %v, want output type error", err)
	}
}

func TestInvokeRecoversPanics(t *testing.T) {
	mem := memory.NewGoAllocator()
	f := Function{
		Name: "panics", Input: Schema(Col("x", arrow.PrimitiveTypes.Int64)), Output: Schema(Col("y", arrow.PrimitiveTypes.Int64)),
		Fn: func(context.Context, memory.Allocator, arrow.RecordBatch) (arrow.RecordBatch, error) { panic("kaboom") },
	}
	in := ipcFromJSON(t, mem, f.Input, `[{"x":1}]`)
	_, err := Invoke(context.Background(), mem, f, in)
	if err == nil || !strings.Contains(err.Error(), "panicked: kaboom") {
		t.Fatalf("got %v, want panic error", err)
	}
}

func TestRegisterValidates(t *testing.T) {
	cases := map[string]Function{
		"bad name":  Map1("has space", "x", "y", func(x int64) (int64, error) { return x, nil }),
		"nil fn":    {Name: "nilfn", Input: Schema(), Output: Schema(Col("y", arrow.PrimitiveTypes.Int64))},
		"bad type":  {Name: "badtype", Input: Schema(Col("x", arrow.BinaryTypes.StringView)), Output: Schema(Col("y", arrow.PrimitiveTypes.Int64)), Fn: func(context.Context, memory.Allocator, arrow.RecordBatch) (arrow.RecordBatch, error) { return nil, nil }},
		"no output": {Name: "noout", Input: Schema(), Output: Schema(), Fn: func(context.Context, memory.Allocator, arrow.RecordBatch) (arrow.RecordBatch, error) { return nil, nil }},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Register did not panic")
				}
			}()
			Register(f)
		})
	}
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom = boomError{}

func TestInvokeMap9(t *testing.T) {
	mem := memory.NewCheckedAllocator(memory.NewGoAllocator())
	defer mem.AssertSize(t, 0)

	f := Map9("nine", "a", "b", "c", "d", "e", "f", "g", "h", "i", "out",
		func(a int8, b int16, c int32, d int64, e uint32, f float32, g float64, h bool, i string) (string, error) {
			return strings.TrimSpace(fmt.Sprintln(a, b, c, d, e, f, g, h, i)), nil
		})
	if got := f.Input.NumFields(); got != 9 {
		t.Fatalf("input has %d columns, want 9", got)
	}
	if got := f.Input.Field(7).Type; !arrow.TypeEqual(got, arrow.FixedWidthTypes.Boolean) {
		t.Fatalf("column h has type %s, want bool", got)
	}
	in := ipcFromJSON(t, mem, f.Input, `[
		{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6.5,"g":7.25,"h":true,"i":"x"},
		{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6.5,"g":7.25,"h":true,"i":null}]`)
	out, err := Invoke(context.Background(), mem, f, in)
	if err != nil {
		t.Fatal(err)
	}
	got := jsonFromIPC(t, out)
	want := "{\"out\":\"1 2 3 4 5 6.5 7.25 true x\"}\n{\"out\":null}"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
