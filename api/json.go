package api

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// JSONToProto converts a json.RawMessage to a *structpb.Value for the wire.
// A nil or empty raw message produces structpb.NullValue.
func JSONToProto(raw json.RawMessage) (*structpb.Value, error) {
	if len(raw) == 0 {
		return structpb.NewNullValue(), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("unmarshal json.RawMessage: %w", err)
	}
	pv, err := structpb.NewValue(v)
	if err != nil {
		return nil, fmt.Errorf("structpb.NewValue: %w", err)
	}
	return pv, nil
}

// ProtoToJSON converts a *structpb.Value from the wire back to json.RawMessage.
// A nil value returns nil.
func ProtoToJSON(pv *structpb.Value) (json.RawMessage, error) {
	if pv == nil {
		return nil, nil
	}
	b, err := pv.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal structpb.Value: %w", err)
	}
	return json.RawMessage(b), nil
}
