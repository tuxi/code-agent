package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"code-agent/internal/agent"
)

// protocolVersion is the agent-wire major version. Negotiated once at the
// connection handshake (Hello), never carried per-event. See
// docs/protocols/agent-wire-v1.md.
const protocolVersion = 1

// Encode renders a core event into one agent-wire JSON frame. eventID and
// parentSessionID are stamped here (transport identity) so toWire stays pure and
// golden-testable. parentSessionID is "" for root-session events.
func Encode(e agent.Event, eventID, parentSessionID string) ([]byte, error) {
	w := toWire(e)
	w.EventID = eventID
	w.ParentSessionID = parentSessionID
	return json.Marshal(w)
}

type helloFrame struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	Server          string `json:"server,omitempty"`
}

// Hello is the first frame on every connection: it pins the protocol version
// once so events never need to carry it.
func Hello(server string) ([]byte, error) {
	return json.Marshal(helloFrame{Type: "hello", ProtocolVersion: protocolVersion, Server: server})
}

// newEventID returns a unique, sortable-enough id for an event. Uniqueness (not
// ordering) is the contract — clients use it for dedup and replay.
func newEventID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}
