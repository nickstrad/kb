package stats

import (
	"strings"
	"testing"
)

// TestApplyPercentiles covers the sample-array edges (empty, all null, one value) and each way a
// query can hand back a malformed percentile column.
func TestApplyPercentiles(t *testing.T) {
	p := []Pct{{Column: "p50", P: 0.5, Digits: 1}}
	for _, tc := range []struct {
		name    string
		cols    []string
		cell    any
		want    any
		wantErr string
	}{
		{"empty array: NULL", []string{"p50"}, "[]", nil, ""},
		{"only nulls: NULL", []string{"p50"}, "[null,null]", nil, ""},
		{"one value", []string{"p50"}, "[4.25]", 4.3, ""},
		{"nulls skipped", []string{"p50"}, "[null,1,3]", 2.0, ""},
		{"column missing", []string{"other"}, "[]", nil, `percentile column "p50" not in result`},
		{"not text", []string{"p50"}, int64(3), nil, "want a JSON array"},
		{"bad JSON", []string{"p50"}, "[1,", nil, `percentile column "p50"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := &Table{Columns: tc.cols, Rows: [][]any{{tc.cell}}}
			err := applyPercentiles(tbl, p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := tbl.Rows[0][0]; got != tc.want {
				t.Fatalf("cell = %#v, want %#v", got, tc.want)
			}
		})
	}
}
