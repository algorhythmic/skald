package archivefeed

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestSharedContractSchemasValidateSyntheticFixtures(t *testing.T) {
	for _, tc := range []struct{ schema, fixture string }{
		{"../schemas/archive-feed-v1.schema.json", "../testdata/archive-feed/page-v1.json"},
		{"../schemas/redaction-receipt-v1.schema.json", "../testdata/redaction-receipt/receipt-v1.json"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			schemaBytes, err := os.ReadFile(tc.schema)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(schemaBytes, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			fixture, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(fixture, &instance); err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(instance); err != nil {
				t.Fatalf("fixture violates schema: %v", err)
			}
			if tc.schema == "../schemas/archive-feed-v1.schema.json" {
				if _, err := DecodePage(fixture); err != nil {
					t.Fatalf("fixture violates Go decoder: %v", err)
				}
			}
		})
	}
}
