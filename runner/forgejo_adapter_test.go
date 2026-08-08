package runner

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

func TestStructToMapFlattensNested(t *testing.T) {
	s, err := structpb.NewStruct(map[string]any{
		"repository": "dbl/site",
		"event": map[string]any{
			"pull_request": map[string]any{
				"number": float64(143),
			},
		},
	})
	if err != nil {
		t.Fatalf("struct: %v", err)
	}
	m := structToMap(s)
	if m["repository"] != "dbl/site" {
		t.Fatalf("repository = %q, want dbl/site", m["repository"])
	}
	if m["event.pull_request.number"] != "143" {
		t.Fatalf("event.pull_request.number = %q, want 143", m["event.pull_request.number"])
	}
}

func TestStructToMapNil(t *testing.T) {
	m := structToMap(nil)
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}
