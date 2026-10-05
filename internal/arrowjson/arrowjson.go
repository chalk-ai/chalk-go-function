// Package arrowjson converts Arrow schemas to and from the protobuf-JSON form
// of chalk.arrow.v1.Schema, which the external function catalog stores as a
// function's input and output schema.
//
// The JSON is produced by hand so that function binaries do not depend on
// Chalk's full generated proto set. Field names are protobuf-JSON lowerCamel;
// parsing also accepts the original snake_case names, as protojson does.
package arrowjson

import (
	"encoding/json"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

// Schema is the JSON form of chalk.arrow.v1.Schema.
type Schema struct {
	Columns []Field `json:"columns"`
}

// Field is the JSON form of chalk.arrow.v1.Field.
type Field struct {
	Name      string         `json:"name"`
	ArrowType map[string]any `json:"arrowType"`
	Nullable  bool           `json:"nullable,omitempty"`
}

var empty = map[string]any{}

// Arrow types whose chalk.arrow.v1.ArrowType variant is an EmptyMessage.
var simpleTypes = []struct {
	key string
	typ arrow.DataType
}{
	{"bool", arrow.FixedWidthTypes.Boolean},
	{"int8", arrow.PrimitiveTypes.Int8},
	{"int16", arrow.PrimitiveTypes.Int16},
	{"int32", arrow.PrimitiveTypes.Int32},
	{"int64", arrow.PrimitiveTypes.Int64},
	{"uint8", arrow.PrimitiveTypes.Uint8},
	{"uint16", arrow.PrimitiveTypes.Uint16},
	{"uint32", arrow.PrimitiveTypes.Uint32},
	{"uint64", arrow.PrimitiveTypes.Uint64},
	{"float16", arrow.FixedWidthTypes.Float16},
	{"float32", arrow.PrimitiveTypes.Float32},
	{"float64", arrow.PrimitiveTypes.Float64},
	{"utf8", arrow.BinaryTypes.String},
	{"largeUtf8", arrow.BinaryTypes.LargeString},
	{"binary", arrow.BinaryTypes.Binary},
	{"largeBinary", arrow.BinaryTypes.LargeBinary},
	{"date32", arrow.FixedWidthTypes.Date32},
	{"date64", arrow.FixedWidthTypes.Date64},
	{"none", arrow.Null},
}

var timeUnits = map[arrow.TimeUnit]string{
	arrow.Second:      "TIME_UNIT_SECOND",
	arrow.Millisecond: "TIME_UNIT_MILLISECOND",
	arrow.Microsecond: "TIME_UNIT_MICROSECOND",
	arrow.Nanosecond:  "TIME_UNIT_NANOSECOND",
}

// FromArrowSchema converts an Arrow schema to its catalog JSON form.
func FromArrowSchema(s *arrow.Schema) (Schema, error) {
	out := Schema{Columns: make([]Field, 0, s.NumFields())}
	for _, f := range s.Fields() {
		jf, err := fromArrowField(f)
		if err != nil {
			return Schema{}, err
		}
		out.Columns = append(out.Columns, jf)
	}
	return out, nil
}

func fromArrowField(f arrow.Field) (Field, error) {
	t, err := TypeJSON(f.Type)
	if err != nil {
		return Field{}, fmt.Errorf("column %q: %w", f.Name, err)
	}
	return Field{Name: f.Name, ArrowType: t, Nullable: f.Nullable}, nil
}

// TypeJSON converts an Arrow data type to the JSON form of chalk.arrow.v1.ArrowType.
func TypeJSON(t arrow.DataType) (map[string]any, error) {
	for _, st := range simpleTypes {
		if arrow.TypeEqual(t, st.typ) {
			return map[string]any{st.key: empty}, nil
		}
	}
	switch t := t.(type) {
	case *arrow.TimestampType:
		return map[string]any{"timestamp": map[string]any{
			"timeUnit": timeUnits[t.Unit],
			"timezone": t.TimeZone,
		}}, nil
	case *arrow.Time32Type:
		return map[string]any{"time32": timeUnits[t.Unit]}, nil
	case *arrow.Time64Type:
		return map[string]any{"time64": timeUnits[t.Unit]}, nil
	case *arrow.DurationType:
		return map[string]any{"duration": timeUnits[t.Unit]}, nil
	case *arrow.FixedSizeBinaryType:
		return map[string]any{"fixedSizeBinary": t.ByteWidth}, nil
	case *arrow.Decimal128Type:
		return map[string]any{"decimal128": map[string]any{"precision": t.Precision, "scale": t.Scale}}, nil
	case *arrow.Decimal256Type:
		return map[string]any{"decimal256": map[string]any{"precision": t.Precision, "scale": t.Scale}}, nil
	case *arrow.ListType:
		elem, err := fromArrowField(t.ElemField())
		if err != nil {
			return nil, err
		}
		return map[string]any{"list": map[string]any{"fieldType": elem}}, nil
	case *arrow.LargeListType:
		elem, err := fromArrowField(t.ElemField())
		if err != nil {
			return nil, err
		}
		return map[string]any{"largeList": map[string]any{"fieldType": elem}}, nil
	case *arrow.FixedSizeListType:
		elem, err := fromArrowField(t.ElemField())
		if err != nil {
			return nil, err
		}
		return map[string]any{"fixedSizeList": map[string]any{"fieldType": elem, "listSize": t.Len()}}, nil
	case *arrow.StructType:
		children := make([]Field, 0, t.NumFields())
		for _, c := range t.Fields() {
			jc, err := fromArrowField(c)
			if err != nil {
				return nil, err
			}
			children = append(children, jc)
		}
		return map[string]any{"struct": map[string]any{"subFieldTypes": children}}, nil
	case *arrow.MapType:
		key, err := fromArrowField(t.KeyField())
		if err != nil {
			return nil, err
		}
		item, err := fromArrowField(t.ItemField())
		if err != nil {
			return nil, err
		}
		return map[string]any{"map": map[string]any{"keyField": key, "itemField": item, "keysSorted": t.KeysSorted}}, nil
	}
	return nil, fmt.Errorf("unsupported arrow type %s", t)
}

// ToArrowSchema parses the catalog JSON form of a schema.
func ToArrowSchema(raw json.RawMessage) (*arrow.Schema, error) {
	var s struct {
		Columns []json.RawMessage `json:"columns"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parsing schema: %w", err)
	}
	fields := make([]arrow.Field, 0, len(s.Columns))
	for _, c := range s.Columns {
		f, err := toArrowField(c)
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
	return arrow.NewSchema(fields, nil), nil
}

func toArrowField(raw json.RawMessage) (arrow.Field, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		return arrow.Field{}, fmt.Errorf("parsing field: %w", err)
	}
	var name string
	var nullable bool
	_ = json.Unmarshal(f["name"], &name)
	_ = json.Unmarshal(f["nullable"], &nullable)
	typRaw := pick(f, "arrowType", "arrow_type")
	if typRaw == nil {
		return arrow.Field{}, fmt.Errorf("field %q has no arrowType", name)
	}
	t, err := toArrowType(typRaw)
	if err != nil {
		return arrow.Field{}, fmt.Errorf("field %q: %w", name, err)
	}
	return arrow.Field{Name: name, Type: t, Nullable: nullable}, nil
}

func toArrowType(raw json.RawMessage) (arrow.DataType, error) {
	var variants map[string]json.RawMessage
	if err := json.Unmarshal(raw, &variants); err != nil {
		return nil, fmt.Errorf("parsing arrowType: %w", err)
	}
	if len(variants) != 1 {
		return nil, fmt.Errorf("arrowType must have exactly one variant, got %d", len(variants))
	}
	var key string
	var body json.RawMessage
	for k, v := range variants {
		key, body = camel(k), v
	}
	for _, st := range simpleTypes {
		if st.key == key {
			return st.typ, nil
		}
	}
	obj := func() map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		return m
	}
	unit := func(raw json.RawMessage) arrow.TimeUnit {
		var s string
		_ = json.Unmarshal(raw, &s)
		for u, name := range timeUnits {
			if name == s {
				return u
			}
		}
		return arrow.Second
	}
	switch key {
	case "timestamp":
		m := obj()
		var tz string
		_ = json.Unmarshal(m["timezone"], &tz)
		return &arrow.TimestampType{Unit: unit(pick(m, "timeUnit", "time_unit")), TimeZone: tz}, nil
	case "time32":
		return &arrow.Time32Type{Unit: unit(body)}, nil
	case "time64":
		return &arrow.Time64Type{Unit: unit(body)}, nil
	case "duration":
		return &arrow.DurationType{Unit: unit(body)}, nil
	case "fixedSizeBinary":
		var n int
		if err := json.Unmarshal(body, &n); err != nil {
			return nil, err
		}
		return &arrow.FixedSizeBinaryType{ByteWidth: n}, nil
	case "decimal128", "decimal256":
		var d struct {
			Precision int32 `json:"precision"`
			Scale     int32 `json:"scale"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		if key == "decimal128" {
			return &arrow.Decimal128Type{Precision: d.Precision, Scale: d.Scale}, nil
		}
		return &arrow.Decimal256Type{Precision: d.Precision, Scale: d.Scale}, nil
	case "list", "largeList", "fixedSizeList":
		m := obj()
		elem, err := toArrowField(pick(m, "fieldType", "field_type"))
		if err != nil {
			return nil, err
		}
		switch key {
		case "list":
			return arrow.ListOfField(elem), nil
		case "largeList":
			return arrow.LargeListOfField(elem), nil
		default:
			var n int32
			_ = json.Unmarshal(pick(m, "listSize", "list_size"), &n)
			return arrow.FixedSizeListOfField(n, elem), nil
		}
	case "struct":
		m := obj()
		var children []json.RawMessage
		_ = json.Unmarshal(pick(m, "subFieldTypes", "sub_field_types"), &children)
		fields := make([]arrow.Field, 0, len(children))
		for _, c := range children {
			f, err := toArrowField(c)
			if err != nil {
				return nil, err
			}
			fields = append(fields, f)
		}
		return arrow.StructOf(fields...), nil
	case "map":
		m := obj()
		k, err := toArrowField(pick(m, "keyField", "key_field"))
		if err != nil {
			return nil, err
		}
		v, err := toArrowField(pick(m, "itemField", "item_field"))
		if err != nil {
			return nil, err
		}
		mt := arrow.MapOf(k.Type, v.Type)
		mt.SetItemNullable(v.Nullable)
		var sorted bool
		_ = json.Unmarshal(pick(m, "keysSorted", "keys_sorted"), &sorted)
		mt.KeysSorted = sorted
		return mt, nil
	}
	return nil, fmt.Errorf("unsupported arrowType variant %q", key)
}

func pick(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// camel converts a proto field name to its protobuf-JSON lowerCamel form.
func camel(s string) string {
	out := make([]byte, 0, len(s))
	upper := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			upper = true
			continue
		}
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		upper = false
		out = append(out, c)
	}
	return string(out)
}
