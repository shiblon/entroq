package version

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Protocol is the EntroQ wire protocol this build's clients speak, and the
// newest its servers serve. It advances when the meaning of the wire changes:
// a new field or mode a server must understand before a client may use it.
//
// Protocol 2 added change modes (TaskChange.mode and DocChange.mode), for
// resetting a task's claim count and for changing only when a task or doc
// set is ready again; doc sets named by key; and claims of doc sets
// (DocClaim.sets).
const Protocol = 2

// ServedProtocols are the protocols this build's servers serve, oldest first.
// Protocol 1 is what a client that sends no protocol speaks, which is every
// client from before the protocol header. Dropping a protocol from this list
// is how a service stops serving the clients that speak it.
var ServedProtocols = []int32{1, 2}

// ProtocolHeader carries the protocol in both directions. A server's
// response lists the protocols it serves ("1,2"); a server that sends none
// serves only protocol 1. A client's request names the one protocol it
// chose, and the server reads the request under it; a request with none is
// protocol 1. VersionHeader names a server's release, for people to read.
const (
	ProtocolHeader = "entroq-protocol"
	VersionHeader  = "entroq-version"
)

// FormatProtocols writes protocols as a ProtocolHeader value.
func FormatProtocols(protocols []int32) string {
	parts := make([]string, len(protocols))
	for i, p := range protocols {
		parts[i] = strconv.Itoa(int(p))
	}
	return strings.Join(parts, ",")
}

// ParseProtocols reads a ProtocolHeader value: one protocol, or a list of
// them separated by commas.
func ParseProtocols(v string) ([]int32, error) {
	var out []int32
	for _, part := range strings.Split(v, ",") {
		p, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || p < 1 {
			return nil, fmt.Errorf("protocol header %q: %q is not a protocol", v, part)
		}
		out = append(out, int32(p))
	}
	return out, nil
}

// Serves reports whether this build's servers serve protocol p.
func Serves(p int32) bool {
	return slices.Contains(ServedProtocols, p)
}
