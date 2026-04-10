package eqsvcjson

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// splicingCodec implements connect.Codec for JSON with splicing for EntroQ bytes.
type splicingCodec struct{}

func newSplicingCodec() connect.Codec {
	return &splicingCodec{}
}

func (c *splicingCodec) Name() string {
	return "json"
}

func (c *splicingCodec) Marshal(v any) ([]byte, error) {
	msg, ok := v.(proto.Message)
	if !ok {
		return json.Marshal(v)
	}

	// First, use standard protojson.
	b, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}

	// Now, splice. We look for "value": "BASE64..." and replace with raw JSON.
	// This is a bit of a hack, but it works for EntroQ's specific structure
	// without needing a full recursive JSON parser during transit.
	// We optimize by checking if the message type is one we care about.
	typeName := string(msg.ProtoReflect().Descriptor().FullName())
	if !strings.Contains(typeName, "api.Task") && !strings.Contains(typeName, "api.TaskData") && !strings.Contains(typeName, "api.ClaimResponse") && !strings.Contains(typeName, "api.TasksResponse") && !strings.Contains(typeName, "api.ModifyResponse") {
		return b, nil
	}

	return spliceValues(b), nil
}

func (c *splicingCodec) Unmarshal(data []byte, v any) error {
	msg, ok := v.(proto.Message)
	if !ok {
		return json.Unmarshal(data, v)
	}

	typeName := string(msg.ProtoReflect().Descriptor().FullName())
	if !strings.Contains(typeName, "api.Task") && !strings.Contains(typeName, "api.TaskData") && !strings.Contains(typeName, "api.ModifyRequest") {
		return protojson.Unmarshal(data, msg)
	}

	// Reverse splice: find raw JSON values and Base64 encode them for protojson.
	processed, err := prepareValuesForProto(data)
	if err != nil {
		return err
	}

	return protojson.Unmarshal(processed, msg)
}

// spliceValues replaces "value":"BASE64" with "value":RAWJSON
func spliceValues(data []byte) []byte {
	var obj any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return data
	}

	if modified := processJSONForOutput(obj); modified {
		newData, err := json.Marshal(obj)
		if err == nil {
			return newData
		}
	}
	return data
}

func processJSONForOutput(v any) bool {
	modified := false
	switch m := v.(type) {
	case map[string]any:
		for k, val := range m {
			if k == "value" {
				if str, ok := val.(string); ok {
					if decoded, err := base64.StdEncoding.DecodeString(str); err == nil && json.Valid(decoded) {
						var rawVal any
						dec := json.NewDecoder(bytes.NewReader(decoded))
						dec.UseNumber()
						if err := dec.Decode(&rawVal); err == nil {
							m[k] = rawVal
							modified = true
							continue
						}
					}
				}
			}
			if processJSONForOutput(val) {
				modified = true
			}
		}
	case []any:
		for _, val := range m {
			if processJSONForOutput(val) {
				modified = true
			}
		}
	}
	return modified
}

// prepareValuesForProto finds "value": {OBJECT} and replaces it with "value": "BASE64"
func prepareValuesForProto(data []byte) ([]byte, error) {
	var obj any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return data, nil
	}

	if modified := processJSONForInput(obj); modified {
		return json.Marshal(obj)
	}
	return data, nil
}

func processJSONForInput(v any) bool {
	modified := false
	switch m := v.(type) {
	case map[string]any:
		for k, val := range m {
			if k == "value" {
				if _, ok := val.(string); !ok {
					// Not a string, try to encode it as Base64 JSON.
					b, err := json.Marshal(val)
					if err == nil {
						m[k] = base64.StdEncoding.EncodeToString(b)
						modified = true
						continue
					}
				}
			}
			if processJSONForInput(val) {
				modified = true
			}
		}
	case []any:
		for _, val := range m {
			if processJSONForInput(val) {
				modified = true
			}
		}
	}
	return modified
}
