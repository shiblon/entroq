package workhost

import (
	"encoding/json"
	"fmt"

	pb "github.com/shiblon/entroq/api"
	"google.golang.org/protobuf/encoding/protojson"
)

// Protocol is the version of this protocol the host speaks. It is the HOST
// GATEWAY's protocol and has nothing to do with version.Protocol, which is what
// clients speak to the EntroQ service: conflating the two caused a real bug
// (23d6697), so they are negotiated separately and never compared.
const Protocol = 2

// What a client sends. Every one of these answers the instruction before it and
// asks for the next, except hello, which only asks.
const (
	msgHello  = "hello"  // what do you support?
	msgConfig = "config" // this is what I am; give me work
	msgDocs   = "docs"   // the doc sets this task needs
	msgResult = "result" // what the work came to
	msgDone   = "done"   // a post-commit phase finished
)

// What the host sends. Every one of these is an instruction.
const (
	insCapabilities = "capabilities" // what I support, who you are, your session
	insTakeDocs     = "takeDocs"     // name the doc sets this task needs
	insDoWork       = "doWork"       // do this task
	insSuccess      = "success"      // the commit landed; here is your hook
	insDependency   = "dependency"   // the commit lost a dependency
	insHold         = "hold"         // nothing yet; ask again
	insDrained      = "drained"      // nothing more is coming; you may stop
	insError        = "error"        // refused, and why
)

// Outcomes a client reports for a task. They map one-to-one onto the Go
// worker's dispositions, so a wire worker has exactly the vocabulary a native
// one does and nothing more.
const (
	outcomeOK    = "ok"    // no error; commit the modification, which may be empty
	outcomeRetry = "retry" // re-queue with backoff, quarantining once attempts run out
	outcomeMove  = "move"  // send straight to a destination queue
	outcomeFatal = "fatal" // stop the whole worker
	outcomeError = "error" // the handler failed in a way it does not understand
)

// The wireX types carry an api/entroq.proto message inside a JSON envelope as
// canonical protojson, so a foreign worker generates its types from the same
// proto the rest of EntroQ uses and hand-models nothing. Only the envelope --
// the type tag and the turn framing -- is this protocol's own.
//
// MarshalJSON is on the value receiver so both direct fields and slice elements
// encode; UnmarshalJSON is on the pointer so the embedded message can be
// allocated.
type wireTask struct{ *pb.Task }

func (w wireTask) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Task) }

func (w *wireTask) UnmarshalJSON(b []byte) error {
	w.Task = new(pb.Task)
	return protojson.Unmarshal(b, w.Task)
}

type wireDoc struct{ *pb.Doc }

func (w wireDoc) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Doc) }

func (w *wireDoc) UnmarshalJSON(b []byte) error {
	w.Doc = new(pb.Doc)
	return protojson.Unmarshal(b, w.Doc)
}

type wireModReq struct{ *pb.ModifyRequest }

func (w wireModReq) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.ModifyRequest) }

func (w *wireModReq) UnmarshalJSON(b []byte) error {
	w.ModifyRequest = new(pb.ModifyRequest)
	return protojson.Unmarshal(b, w.ModifyRequest)
}

// wireSet is a doc set: its lock, carried as a Doc with no ID, and its members.
type wireSet struct {
	Set  wireDoc   `json:"set"`
	Docs []wireDoc `json:"docs,omitempty"`
}

// answer is a client's message: what it is answering, and whether it answers
// anything at all.
//
// answers is false only for hello, which starts the conversation. Every other
// message is the reply to the instruction before it, which is what keeps the
// turns aligned without either side tracking a sequence number.
type answer struct {
	Type string `json:"type"`

	// Drain asks the host to finish the work in hand and then stop. A client
	// traps its own signals and sets this on whatever message it is already
	// about to send, so draining costs no extra turn and lands at a point where
	// the host knows exactly what is outstanding.
	Drain bool `json:"drain,omitempty"`

	// hello
	Protocols []int `json:"protocols,omitempty"`

	// config, which may shadow or append to the registration given out of band
	Config *Config `json:"config,omitempty"`

	// docs
	Claims []DocClaim `json:"claims,omitempty"`

	// result and done
	Outcome      string      `json:"outcome,omitempty"`
	Message      string      `json:"message,omitempty"`
	Modification *wireModReq `json:"modification,omitempty"`

	// answers says this message replies to an instruction a phase is waiting
	// for, which is true of docs, result and done. hello and config ASK without
	// answering: the phase alternation has not started when they are sent, so
	// there is nobody waiting. Set by the decoder, not carried on the wire.
	answers bool `json:"-"`
}

// instruction is what the host sends back: one thing to do next.
type instruction struct {
	Type string `json:"type"`

	// capabilities
	Protocol     int      `json:"protocol,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Session      string   `json:"session,omitempty"`
	Claimant     string   `json:"claimant,omitempty"`

	// takeDocs, doWork, success, dependency
	Task *wireTask   `json:"task,omitempty"`
	Sets []wireSet   `json:"sets,omitempty"`
	Deps *wireModReq `json:"dependencies,omitempty"`

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// DocClaim names one doc set a task needs.
type DocClaim struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// Config is a client's registration: the queues it serves and the phases it
// implements.
//
// It arrives out of band at the base -- URL parameters over HTTP, environment
// and flags for a child process -- and the config message may then shadow or
// append to it, now that the client has seen what the host offers.
//
// The lease is deliberately absent. It is the host's to set: a client that could
// move it could pin a task for as long as it liked.
type Config struct {
	Queues      []string `json:"queues,omitempty"`
	ErrorQueue  string   `json:"errorQueue,omitempty"`
	MaxAttempts int32    `json:"maxAttempts,omitempty"`
	MaxClaims   int32    `json:"maxClaims,omitempty"`

	// TakeDocs says the client names doc sets per task. It stands for the whole
	// session: a client that needs the phase always needs it, so this is
	// registration rather than something asked per task.
	TakeDocs bool `json:"takeDocs,omitempty"`
	// Success and Dependency say the client wants the post-commit hooks.
	Success    bool `json:"success,omitempty"`
	Dependency bool `json:"dependency,omitempty"`
}

// hold tells the client nothing is ready and to ask again. It carries no state:
// a client that loses one has lost nothing.
func hold() instruction { return instruction{Type: insHold} }

// drained tells the client nothing more is coming and it may stop.
func drained() instruction { return instruction{Type: insDrained} }

// refuse tells the client why the host will not go on.
func refuse(code, format string, args ...any) instruction {
	return instruction{Type: insError, Code: code, Message: fmt.Sprintf(format, args...)}
}

// decodeAnswer reads a client message, marking whether it answers a phase that
// is waiting. Only the work messages do: the handshake asks without answering.
func decodeAnswer(b []byte) (answer, error) {
	var a answer
	if err := json.Unmarshal(b, &a); err != nil {
		return answer{}, err
	}
	a.answers = a.Type == msgDocs || a.Type == msgResult || a.Type == msgDone
	return a, nil
}
