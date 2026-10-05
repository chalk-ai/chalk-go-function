package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// columnsToIPC converts a column-oriented JSON object ({"x": [1, 2]}) into an
// Arrow IPC stream with the given schema.
func columnsToIPC(input string, schema *arrow.Schema) ([]byte, error) {
	var cols map[string][]json.RawMessage
	if err := json.Unmarshal([]byte(input), &cols); err != nil {
		return nil, fmt.Errorf("input must be a JSON object of column arrays: %w", err)
	}
	n := -1
	for _, f := range schema.Fields() {
		col, ok := cols[f.Name]
		if !ok {
			return nil, fmt.Errorf("input is missing column %q", f.Name)
		}
		if n >= 0 && len(col) != n {
			return nil, fmt.Errorf("column %q has %d values, other columns have %d", f.Name, len(col), n)
		}
		n = len(col)
	}
	// RecordFromJSON takes rows; transpose the columns into that form.
	rows := make([]map[string]json.RawMessage, max(n, 0))
	for i := range rows {
		rows[i] = make(map[string]json.RawMessage, schema.NumFields())
		for _, f := range schema.Fields() {
			rows[i][f.Name] = cols[f.Name][i]
		}
	}
	rowJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	mem := memory.NewGoAllocator()
	rec, _, err := array.RecordFromJSON(mem, schema, bytes.NewReader(rowJSON))
	if err != nil {
		return nil, fmt.Errorf("converting input to arrow: %w", err)
	}
	defer rec.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema), ipc.WithAllocator(mem))
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ipcToColumns renders an Arrow IPC stream as a column-oriented JSON object,
// concatenating every batch in the stream.
func ipcToColumns(payload []byte) (string, error) {
	r, err := ipc.NewReader(bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer r.Release()
	schema := r.Schema()
	values := make([][]json.RawMessage, schema.NumFields())
	for r.Next() {
		rec := r.RecordBatch()
		for i, col := range rec.Columns() {
			raw, err := col.MarshalJSON()
			if err != nil {
				return "", err
			}
			var vs []json.RawMessage
			if err := json.Unmarshal(raw, &vs); err != nil {
				return "", err
			}
			values[i] = append(values[i], vs...)
		}
	}
	if err := r.Err(); err != nil {
		return "", err
	}
	// Emit columns in schema order rather than Go's sorted map key order.
	var b strings.Builder
	b.WriteString("{")
	for i, f := range schema.Fields() {
		if i > 0 {
			b.WriteString(", ")
		}
		name, _ := json.Marshal(f.Name)
		vs := values[i]
		if vs == nil {
			vs = []json.RawMessage{}
		}
		col, err := json.Marshal(vs)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s: %s", name, col)
	}
	b.WriteString("}")
	return b.String(), nil
}

// emptyIPC is an Arrow IPC stream holding one zero-row batch of schema.
func emptyIPC(schema *arrow.Schema) ([]byte, error) {
	mem := memory.NewGoAllocator()
	b := array.NewRecordBuilder(mem, schema)
	defer b.Release()
	rec := b.NewRecordBatch()
	defer rec.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema), ipc.WithAllocator(mem))
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
