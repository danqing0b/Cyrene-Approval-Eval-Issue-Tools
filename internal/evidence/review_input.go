package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

func projectCase(sample *Sample, suite Suite) {
	var record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(sample.InputRecord))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil || record == nil {
		return
	}
	if id, ok := record["sampleId"].(string); ok && len(id) <= 500 && strings.TrimSpace(id) != "" {
		sample.caseID = id
	}
	reference, exists := record[suite.ExpectedField]
	if !exists || reference == nil {
		return
	}
	// Preserve every field except the output scored by this suite and Echo's
	// documented output provenance. Model, endpoint and usage are not test inputs.
	delete(record, suite.ActualField)
	for _, field := range []string{"model", "endpoint", "usage", "usageRef", "modelMetadata", "modelMetadataRef"} {
		if field != suite.ExpectedField {
			delete(record, field)
		}
	}
	for _, field := range []string{"instruction", "input", "prompt", "messages", "question", "query"} {
		if field != suite.ExpectedField && hasContent(record[field]) {
			sample.hasInput = true
			break
		}
	}
	// Check the persisted reference independently when Echo supplied it.
	if sample.Expected != nil {
		var evaluatedReference any
		decoder := json.NewDecoder(bytes.NewReader(sample.Expected))
		decoder.UseNumber()
		if err := decoder.Decode(&evaluatedReference); err != nil || evaluatedReference == nil {
			return
		}
		left, leftErr := json.Marshal(reference)
		right, rightErr := json.Marshal(evaluatedReference)
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return
		}
	}
	canonical, err := json.Marshal(record)
	if err == nil {
		sample.caseDigest = fmt.Sprintf("%x", sha256.Sum256(canonical))
	}
}

func hasContent(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, item := range v {
			if hasContent(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if hasContent(item) {
				return true
			}
		}
	}
	return false
}
