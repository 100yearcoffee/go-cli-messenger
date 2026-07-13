package protocol

import (
	"encoding/json"
	"time"
)

const (
	MaxControlMessageSize = 16 << 10
	MaxChatTextSize       = 4 << 10
)

type ControlType string

const (
	ControlPeerHello     ControlType = "peer.hello"
	ControlChatMessage   ControlType = "chat.message"
	ControlVideoKeyframe ControlType = "video.keyframe_request"
	ControlAudioState    ControlType = "audio.state"
	ControlSessionHangup ControlType = "session.hangup"
)

var knownControlTypes = map[ControlType]struct{}{
	ControlPeerHello: {}, ControlChatMessage: {}, ControlVideoKeyframe: {}, ControlAudioState: {}, ControlSessionHangup: {},
}

type ControlMessage struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    ControlType     `json:"type"`
	SentAt  time.Time       `json:"sent_at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type ChatPayload struct {
	Text string `json:"text"`
}

type Capabilities struct {
	TextChat     bool `json:"text_chat"`
	ASCIIVideo   bool `json:"ascii_video"`
	ASCIIColumns int  `json:"ascii_columns,omitempty"`
	ASCIIRows    int  `json:"ascii_rows,omitempty"`
	ASCIIFPS     int  `json:"ascii_fps,omitempty"`
	Audio        bool `json:"audio"`
}

type PeerHelloPayload struct {
	Capabilities Capabilities `json:"capabilities"`
}

type AudioStatePayload struct {
	Muted bool `json:"muted"`
}

func (t ControlType) IsKnown() bool {
	_, ok := knownControlTypes[t]
	return ok
}
