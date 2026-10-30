package local

import (
	"fmt"
	"sort"
	"strings"
)

// Spec describes one supported model: its Hugging Face id, vector width, and the task prefixes
// its authors recommend. rembed adds no prefixes itself, so getting these wrong silently lowers
// retrieval quality.
type Spec struct {
	ID          string
	Dim         int
	QueryPrefix string
	DocPrefix   string
}

// DefaultModel is the model kb uses when the config names none.
const DefaultModel = "BAAI/bge-small-en-v1.5"

var models = map[string]Spec{
	"BAAI/bge-small-en-v1.5": {
		ID: "BAAI/bge-small-en-v1.5", Dim: 384,
		QueryPrefix: "Represent this sentence for searching relevant passages: ",
	},
	"BAAI/bge-base-en-v1.5": {
		ID: "BAAI/bge-base-en-v1.5", Dim: 768,
		QueryPrefix: "Represent this sentence for searching relevant passages: ",
	},
	"sentence-transformers/all-MiniLM-L6-v2": {
		ID: "sentence-transformers/all-MiniLM-L6-v2", Dim: 384,
	},
	"nomic-ai/nomic-embed-text-v1.5": {
		ID: "nomic-ai/nomic-embed-text-v1.5", Dim: 768,
		QueryPrefix: "search_query: ", DocPrefix: "search_document: ",
	},
}

// Lookup returns the spec for a model id. Only models in the table are accepted: kb needs the
// vector width before the model is loaded, and the prefixes are model-specific.
func Lookup(id string) (Spec, error) {
	if s, ok := models[id]; ok {
		return s, nil
	}
	return Spec{}, fmt.Errorf("unknown embedding model %q (supported: %s)", id, strings.Join(Supported(), ", "))
}

// Supported lists the model ids kb knows, sorted.
func Supported() []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
