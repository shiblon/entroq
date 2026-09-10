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
			ReplyQueue: "/service/sess=session-1;gc=123/reply",
		},
		Method: "POST",
		Path:   "/items",
		Body:   []byte{0x00, 0xff, 0x80, 'A'},
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
	for _, field := range []string{"session", "reply_queue", "method", "path", "body"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("missing top-level field %q in %s", field, value)
		}
	}
}

func TestFinalResponseOmitsReplyQueue(t *testing.T) {
	value, err := json.Marshal(Response{
		FrameControl: FrameControl{
			Session: "session-1",
			Final:   true,
		},
		StatusCode: 204,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["reply_queue"]; ok {
		t.Fatalf("final response has reply queue: %s", value)
	}
	if _, ok := fields["final"]; !ok {
		t.Fatalf("final response omits final marker: %s", value)
	}
}
