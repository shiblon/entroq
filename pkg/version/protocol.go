package version

// Protocol is the EntroQ wire protocol this build speaks. It advances when
// the wire gains something a client must know the server understands before
// sending it; a server that sends no protocol header speaks protocol 1.
//
// Protocol 2 added lease-only task and doc set changes (new_lease), changes
// that reset claims (new_zero_claims), doc sets named by key, and claims of
// doc sets (DocClaim.sets). A 1.12 server applies a change carrying only a new
// field as one with empty data, so a client must not send these to a server
// below protocol 2.
const Protocol = 2

// Response headers every server sends: the protocol it speaks, and its
// release, for people to read.
const (
	ProtocolHeader = "entroq-protocol"
	VersionHeader  = "entroq-version"
)
