package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	ProtocolVersion      = 2
	MaxSignalMessageSize = 64 << 10
	MaxSDPMessageSize    = 256 << 10
	MaxICECandidateSize  = 4 << 10
	MaxICECandidates     = 256
	MinBaseNameLength    = 3
	MaxBaseNameLength    = 19
	FingerprintLength    = 52
	AddressSuffixLength  = 12
	MaxAddressLength     = MaxBaseNameLength + 1 + AddressSuffixLength
)

type SignalType string

const (
	SignalSessionHello      SignalType = "session.hello"
	SignalSessionReady      SignalType = "session.ready"
	SignalSessionPing       SignalType = "session.ping"
	SignalSessionPong       SignalType = "session.pong"
	SignalSessionError      SignalType = "session.error"
	SignalPresenceQuery     SignalType = "presence.query"
	SignalPresenceResult    SignalType = "presence.result"
	SignalCallInvite        SignalType = "call.invite"
	SignalCallRinging       SignalType = "call.ringing"
	SignalCallAccept        SignalType = "call.accept"
	SignalCallDecline       SignalType = "call.decline"
	SignalCallCancel        SignalType = "call.cancel"
	SignalCallBusy          SignalType = "call.busy"
	SignalCallTimeout       SignalType = "call.timeout"
	SignalCallEnd           SignalType = "call.end"
	SignalWebRTCOffer       SignalType = "webrtc.offer"
	SignalWebRTCAnswer      SignalType = "webrtc.answer"
	SignalWebRTCICE         SignalType = "webrtc.ice"
	SignalWebRTCICEComplete SignalType = "webrtc.ice_complete"
)

var knownSignalTypes = map[SignalType]struct{}{
	SignalSessionHello: {}, SignalSessionReady: {}, SignalSessionPing: {},
	SignalSessionPong: {}, SignalSessionError: {}, SignalPresenceQuery: {},
	SignalPresenceResult: {}, SignalCallInvite: {}, SignalCallRinging: {},
	SignalCallAccept: {}, SignalCallDecline: {}, SignalCallCancel: {},
	SignalCallBusy: {}, SignalCallTimeout: {}, SignalCallEnd: {},
	SignalWebRTCOffer: {}, SignalWebRTCAnswer: {}, SignalWebRTCICE: {},
	SignalWebRTCICEComplete: {},
}

type SignalMessage struct {
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Type      SignalType      `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	CallID    string          `json:"call_id,omitempty"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type SDPPayload struct {
	SDP string `json:"sdp"`
}

type ICEPayload struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdp_mline_index,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PresencePayload struct {
	Online bool `json:"online"`
}

// IdentityProof proves control of the private key behind a canonical address.
// Nonces are single-use until ExpiresAt.
type IdentityProof struct {
	PublicKey string    `json:"public_key"`
	ExpiresAt time.Time `json:"expires_at"`
	Nonce     string    `json:"nonce"`
	Signature string    `json:"signature"`
}

type SessionReadyPayload struct {
	STUNURLs []string `json:"stun_urls,omitempty"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type TURNCredentials struct {
	ExpiresAt  time.Time   `json:"expires_at"`
	ICEServers []ICEServer `json:"ice_servers"`
}

func (t SignalType) IsKnown() bool {
	_, ok := knownSignalTypes[t]
	return ok
}

func (t SignalType) IsSDP() bool {
	return t == SignalWebRTCOffer || t == SignalWebRTCAnswer
}

func (t SignalType) RequiresCallID() bool {
	switch t {
	case SignalCallInvite, SignalCallRinging, SignalCallAccept, SignalCallDecline,
		SignalCallCancel, SignalCallBusy, SignalCallTimeout, SignalCallEnd,
		SignalWebRTCOffer, SignalWebRTCAnswer, SignalWebRTCICE, SignalWebRTCICEComplete:
		return true
	default:
		return false
	}
}

func ValidSTUNURL(value string) bool {
	return len(value) >= 8 && len(value) <= 512 &&
		(strings.HasPrefix(value, "stun:") || strings.HasPrefix(value, "stuns:")) &&
		!strings.ContainsAny(value, "\r\n\t ")
}

func ValidTURNURL(value string) bool {
	return len(value) >= 8 && len(value) <= 512 &&
		(strings.HasPrefix(value, "turn:") || strings.HasPrefix(value, "turns:")) &&
		!strings.ContainsAny(value, "\r\n\t ")
}
