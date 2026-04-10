package eqsvcjson

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/shiblon/entroq/api"
)

func TestSplicingCodec_Marshal(t *testing.T) {
	codec := newSplicingCodec()
	
	// A value with a large integer that would lose precision in float64.
	largeIntJSON := `{"id":1234567890123456789}`
	valueBytes := []byte(largeIntJSON)
	
	task := &pb.Task{
		Id:    "test-id",
		Value: valueBytes,
	}
	
	data, err := codec.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	
	jsonStr := string(data)
	t.Logf("Marshaled JSON: %s", jsonStr)
	
	// Verify it contains the raw large integer.
	if !strings.Contains(jsonStr, largeIntJSON) {
		t.Errorf("Expected raw JSON %q in output, but got %q", largeIntJSON, jsonStr)
	}
	
	// Verify it does NOT contain the Base64 version.
	b64 := base64.StdEncoding.EncodeToString(valueBytes)
	if strings.Contains(jsonStr, b64) {
		t.Errorf("Found Base64 version %q in output, expected raw JSON", b64)
	}
}

func TestSplicingCodec_Unmarshal(t *testing.T) {
	codec := newSplicingCodec()
	
	// Input JSON with a raw large integer.
	inputJSON := `{"id":"test-id","value":{"id":1234567890123456789}}`
	
	task := new(pb.Task)
	if err := codec.Unmarshal([]byte(inputJSON), task); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	
	// Verify the value bytes are correct.
	expectedValue := `{"id":1234567890123456789}`
	if string(task.Value) != expectedValue {
		t.Errorf("Expected value %q, got %q", expectedValue, string(task.Value))
	}
	
	// Verify we can parse the resulting bytes back into an int64 without loss.
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(task.Value, &result); err != nil {
		t.Fatalf("Failed to unmarshal bytes back to struct: %v", err)
	}
	if result.ID != 1234567890123456789 {
		t.Errorf("Precision loss! Expected 1234567890123456789, got %d", result.ID)
	}
}
