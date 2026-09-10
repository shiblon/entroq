package async

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	want := Envelope{
		FrameControl: FrameControl{
			Session:    "session-1",
			ReplyQueue: "/service/sess=session-1;gc=123/request-ack",
		},
		ResponseQueue: "/service/sess=session-1;gc=123/response-data",
		Method:        "POST",
		Path:          "/items",
		ProtocolMajor: 2,
		ContentLength: 4,
		TrailerKeys:   []string{"Request-Checksum"},
		Body:          []byte{0x00, 0xff, 0x80, 'A'},
	}

	value, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(value, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Session != want.Session {
		t.Errorf("session: got %q, want %q", got.Session, want.Session)
	}
	if got.ReplyQueue != want.ReplyQueue {
		t.Errorf("reply queue: got %q, want %q", got.ReplyQueue, want.ReplyQueue)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Errorf("body: got %x, want %x", got.Body, want.Body)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	for _, field := range []string{"session", "reply_queue", "response_queue", "method", "path", "protocol_major", "content_length", "trailer_keys", "body"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("missing top-level field %q in %s", field, value)
		}
	}
}

func TestTerminalResponseOmitsReplyQueue(t *testing.T) {
	value, err := json.Marshal(Response{
		FrameControl: FrameControl{
			Session: "session-1",
			Final:   true,
		},
		StatusCode: 204,
		Trailers:   map[string][]string{"Grpc-Status": {"0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["reply_queue"]; ok {
		t.Fatalf("terminal response has reply queue: %s", value)
	}
	if _, ok := fields["final"]; !ok {
		t.Fatalf("terminal response omits final marker: %s", value)
	}
}

func TestEmptyResponseCarriesOnlyLaneControl(t *testing.T) {
	value, err := json.Marshal(Response{FrameControl: FrameControl{
		Session:    "session-1",
		ReplyQueue: "/service/sess=session-1;gc=123/response-data",
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("empty response fields: got %v, want only session and reply_queue", fields)
	}
}

func TestCopyHeadersPreservesOnlyTrailersTE(t *testing.T) {
	got := copyHeaders(map[string][]string{
		"Te":         {"gzip", "trailers"},
		"Connection": {"close"},
		"X-Test":     {"one"},
	})
	if values := got.Values("Te"); len(values) != 1 || values[0] != "trailers" {
		t.Errorf("TE: got %v, want [trailers]", values)
	}
	if got.Get("Connection") != "" {
		t.Errorf("Connection was forwarded: %v", got.Values("Connection"))
	}
	if got.Get("X-Test") != "one" {
		t.Errorf("X-Test: got %q, want one", got.Get("X-Test"))
	}
}
