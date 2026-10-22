package eqsvcjson

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A JSON request with a field this server does not know is refused rather
// than read without it, as a binary one is by the service: a client may
// ignore what it does not understand, a server may not. Both codecs below
// differ from their libraries' defaults only in that.

// unknownFieldError names a JSON decoding failure for what it most likely is.
func unknownFieldError(err error) error {
	return fmt.Errorf("request does not match what this server, %s, serves (protocols %s): %w",
		version.Version, version.FormatProtocols(version.ServedProtocols), err)
}

// strictJSONCodec is connect's JSON codec, refusing unknown fields.
type strictJSONCodec struct{ name string }

func (c strictJSONCodec) Name() string { return c.name }

func (c strictJSONCodec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("%T is not a proto message", message)
	}
	return protojson.MarshalOptions{}.Marshal(m)
}

func (c strictJSONCodec) Unmarshal(data []byte, message any) error {
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("%T is not a proto message", message)
	}
	if len(data) == 0 {
		return errors.New("zero-length payload is not a valid JSON object")
	}
	if err := (protojson.UnmarshalOptions{}).Unmarshal(data, m); err != nil {
		return unknownFieldError(fmt.Errorf("unmarshal into %T: %w", message, err))
	}
	return nil
}

// strictCodecs returns the connect handler options that install the strict
// JSON codec under both names connect serves JSON by.
func strictCodecs() []connect.HandlerOption {
	return []connect.HandlerOption{
		connect.WithCodec(strictJSONCodec{name: "json"}),
		connect.WithCodec(strictJSONCodec{name: "json; charset=utf-8"}),
	}
}

// strictRESTCodec is vanguard's JSON codec for the REST routes, refusing
// unknown fields.
func strictRESTCodec(res vanguard.TypeResolver) vanguard.Codec {
	c := vanguard.NewJSONCodec(res)
	c.UnmarshalOptions.DiscardUnknown = false
	return strictREST{c}
}

// strictREST wraps vanguard's JSON codec so its decoding errors say what a
// refused field most likely means.
type strictREST struct{ *vanguard.JSONCodec }

func (c strictREST) Unmarshal(data []byte, msg proto.Message) error {
	if err := c.JSONCodec.Unmarshal(data, msg); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, unknownFieldError(err))
	}
	return nil
}

func (c strictREST) UnmarshalField(data []byte, msg proto.Message, field protoreflect.FieldDescriptor) error {
	if err := c.JSONCodec.UnmarshalField(data, msg, field); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, unknownFieldError(err))
	}
	return nil
}
