package async

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

func TestHeartbeatTimingUsesThirdOfPeerTimeout(t *testing.T) {
	timing := heartbeatTimingForTimeout(3 * time.Minute)
	if got, want := timing.after, time.Minute; got != want {
		t.Errorf("heartbeat interval: got %v, want %v", got, want)
	}
	if got, want := timing.timeout, 3*time.Minute; got != want {
		t.Errorf("peer timeout: got %v, want %v", got, want)
	}
}

func TestRequestAcknowledgementRejectsApplicationData(t *testing.T) {
	session := &senderSession{session: "session-1"}
	_, err := session.handleRequestAck(context.Background(), nil, Envelope{
		FrameControl: FrameControl{
			Session:    "session-1",
			ReplyQueue: "/service/request-data",
		},
		Body: []byte("not an acknowledgement"),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "where an ACK was expected") {
		t.Fatalf("error: got %v, want ACK-shape protocol error", err)
	}
}

func TestResponseAcknowledgementRejectsApplicationData(t *testing.T) {
	receiver := &Receiver{}
	handler := receiver.responseHandler(sessionStart{}, &receiverSessionState{}, newResponseSocket(), func() {})
	_, err := handler(context.Background(), nil, Response{
		FrameControl: FrameControl{
			Session:    "session-1",
			ReplyQueue: "/service/response-data",
		},
		Body: []byte("not an acknowledgement"),
	}, []*entroq.Doc{{}})
	if err == nil || !strings.Contains(err.Error(), "where an ACK was expected") {
		t.Fatalf("error: got %v, want ACK-shape protocol error", err)
	}
}
