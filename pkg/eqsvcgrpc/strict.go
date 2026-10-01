package eqsvcgrpc

import (
	"fmt"
	"strings"

	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// checkKnown refuses a request carrying fields this server does not know.
// Decoding keeps such fields aside rather than failing, and a server that
// ignored them would do something other than what the client asked: a
// client may ignore what it does not understand, a server may not. Protocol
// negotiation refuses a client that declares a protocol the server does not
// serve; this catches one whose request does not match what it declared.
func checkKnown(req proto.Message, protocol int32) error {
	var unknown []string
	unknownFields(req.ProtoReflect(), make([]step, 0, 8), &unknown)
	if len(unknown) == 0 {
		return nil
	}
	return codeErrorf(codes.InvalidArgument,
		"request declares protocol %d but carries fields this server does not know (%s); this server, %s, serves protocols %s",
		protocol, strings.Join(unknown, ", "), version.Version, version.FormatProtocols(version.ServedProtocols))
}

// step is one step of the path to a message within a request: a field, and
// for a list element its index, or for a map entry its key.
type step struct {
	field string
	index int // a list element's index, or -1
	key   any // a map entry's key, or nil
}

// unknownFields appends to out the path and number of every unknown field in
// m and the messages within it. It visits only the fields that are set, and
// writes a path only for a field it reports, so a request with none costs
// little more than reading it.
func unknownFields(m protoreflect.Message, path []step, out *[]string) {
	if raw := m.GetUnknown(); len(raw) > 0 {
		for _, num := range unknownNumbers(raw) {
			*out = append(*out, fmt.Sprintf("%sfield %d", format(path), num))
		}
	}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Message() == nil || !m.Has(fd) {
			continue
		}
		name := string(fd.Name())
		switch {
		case fd.IsList():
			list := m.Get(fd).List()
			for j := range list.Len() {
				unknownFields(list.Get(j).Message(), append(path, step{name, j, nil}), out)
			}
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				continue
			}
			m.Get(fd).Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				unknownFields(mv.Message(), append(path, step{name, -1, k.Interface()}), out)
				return true
			})
		default:
			unknownFields(m.Get(fd).Message(), append(path, step{name, -1, nil}), out)
		}
	}
}

// format writes path as a field path, followed by a space, or nothing for
// the request itself.
func format(path []step) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	for i, st := range path {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(st.field)
		switch {
		case st.index >= 0:
			fmt.Fprintf(&b, "[%d]", st.index)
		case st.key != nil:
			fmt.Fprintf(&b, "[%v]", st.key)
		}
	}
	b.WriteByte(' ')
	return b.String()
}

// unknownNumbers returns the field numbers in raw unknown-field bytes.
func unknownNumbers(raw protoreflect.RawFields) []protowire.Number {
	var nums []protowire.Number
	for len(raw) > 0 {
		num, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			break
		}
		nums = append(nums, num)
		raw = raw[n:]
	}
	return nums
}
