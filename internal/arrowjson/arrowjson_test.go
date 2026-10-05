package arrowjson

import (
	"encoding/json"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

func TestRoundTrip(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.LargeString},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, Nullable: true},
		{Name: "d", Type: arrow.FixedWidthTypes.Date32},
		{Name: "dec", Type: &arrow.Decimal128Type{Precision: 18, Scale: 4}},
		{Name: "l", Type: arrow.ListOf(arrow.PrimitiveTypes.Float64), Nullable: true},
		{Name: "st", Type: arrow.StructOf(
			arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
			arrow.Field{Name: "b", Type: arrow.LargeListOf(arrow.BinaryTypes.String)},
		)},
		{Name: "m", Type: arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int64)},
	}, nil)
	js, err := FromArrowSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(js)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ToArrowSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(schema) {
		t.Fatalf("round trip mismatch:\n got %s\nwant %s\njson %s", back, schema, raw)
	}
}

func TestParsesSnakeCase(t *testing.T) {
	raw := `{"columns":[{"name":"x","arrow_type":{"large_utf8":{}},"nullable":true},
		{"name":"t","arrowType":{"timestamp":{"time_unit":"TIME_UNIT_NANOSECOND"}}}]}`
	s, err := ToArrowSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := arrow.NewSchema([]arrow.Field{
		{Name: "x", Type: arrow.BinaryTypes.LargeString, Nullable: true},
		{Name: "t", Type: &arrow.TimestampType{Unit: arrow.Nanosecond}},
	}, nil)
	if !s.Equal(want) {
		t.Fatalf("got %s, want %s", s, want)
	}
}
