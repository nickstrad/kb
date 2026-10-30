package cli

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

// failWriter is a stdout whose writes fail, as a closed pipe does.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestEncodeJSONWriteError: a failed stdout write is Internal, whatever the value encoded
// (CODING_STYLE §1.5).
func TestEncodeJSONWriteError(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"object", map[string]int{"entries": 2}},
		{"scalar", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := encodeJSON(json.NewEncoder(failWriter{}), tc.v, "cli.test")
			if errs.KindOf(err) != errs.Internal {
				t.Fatalf("encodeJSON error = %v (kind %v), want kind Internal", err, errs.KindOf(err))
			}
		})
	}
}
