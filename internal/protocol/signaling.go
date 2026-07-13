package protocol

import (
	"encoding/json"
	"time"
)

const (
	ProtocolVersion      = 1
	MaxSignalMessageSize = 64 << 10
	MaxSDPMessageSize    = 256 << 10
	MaxICECandidateSize  = 4 << 10
	MaxICECandidates     = 256
	MinUsernameLength    = 3
	MaxUsernameLength    = 32
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
