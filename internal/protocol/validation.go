package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrMessageTooLarge    = errors.New("message too large")
	ErrUnsupportedVersion = errors.New("unsupported protocol version")
	ErrInvalidMessage     = errors.New("invalid protocol message")

	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

type Validator struct {
	Now           func() time.Time
	MaximumAge    time.Duration
	MaximumFuture time.Duration
}

func NewValidator() Validator {
	return Validator{
		Now:           time.Now,
		MaximumAge:    24 * time.Hour,
		MaximumFuture: 5 * time.Minute,
	}
}

func DecodeSignal(data []byte, validator Validator) (SignalMessage, error) {
	if len(data) > MaxSDPMessageSize {
		return SignalMessage{}, fmt.Errorf("%w: signaling message is %d bytes", ErrMessageTooLarge, len(data))
	}

	var message SignalMessage
	if err := decodeJSON(data, &message); err != nil {
		return SignalMessage{}, fmt.Errorf("%w: decode signaling JSON: %v", ErrInvalidMessage, err)
	}
	limit := MaxSignalMessageSize
	if message.Type.IsSDP() {
		limit = MaxSDPMessageSize
	}
	if len(data) > limit {
		return SignalMessage{}, fmt.Errorf("%w: %s message is %d bytes", ErrMessageTooLarge, message.Type, len(data))
	}
	if err := validator.ValidateSignal(message); err != nil {
		return SignalMessage{}, err
	}
	return message, nil
}

func DecodeControl(data []byte, validator Validator) (ControlMessage, error) {
	if len(data) > MaxControlMessageSize {
		return ControlMessage{}, fmt.Errorf("%w: control message is %d bytes", ErrMessageTooLarge, len(data))
	}

	var message ControlMessage
	if err := decodeJSON(data, &message); err != nil {
		return ControlMessage{}, fmt.Errorf("%w: decode control JSON: %v", ErrInvalidMessage, err)
	}
	if err := validator.ValidateControl(message); err != nil {
		return ControlMessage{}, err
	}
	return message, nil
}

func (v Validator) ValidateSignal(message SignalMessage) error {
	if err := v.validateCommon(message.Version, message.ID, message.Timestamp); err != nil {
		return err
	}
	if !message.Type.IsKnown() {
		return invalid("unknown signaling type %q", message.Type)
	}
	if message.From != "" && !ValidUsername(message.From) {
		return invalid("invalid sender username %q", message.From)
	}
	if message.To != "" && !ValidUsername(message.To) {
		return invalid("invalid recipient username %q", message.To)
	}
	if message.Type.RequiresCallID() && !validUUID(message.CallID) {
		return invalid("type %q requires a valid call ID", message.Type)
	}

	switch message.Type {
	case SignalSessionHello:
		if message.From == "" {
			return invalid("session.hello requires a sender")
		}
	case SignalSessionReady, SignalSessionError:
		if message.To == "" {
			return invalid("%s requires a recipient", message.Type)
		}
	default:
		if message.From == "" || message.To == "" {
			return invalid("%s requires sender and recipient", message.Type)
		}
	}

	switch message.Type {
	case SignalWebRTCOffer, SignalWebRTCAnswer:
		var payload SDPPayload
		if err := requiredPayload(message.Payload, &payload); err != nil || strings.TrimSpace(payload.SDP) == "" {
			return invalid("%s requires a non-empty SDP payload", message.Type)
		}
	case SignalWebRTCICE:
		var payload ICEPayload
		if err := requiredPayload(message.Payload, &payload); err != nil {
			return invalid("webrtc.ice requires a candidate payload")
		}
		if payload.Candidate == "" || len(payload.Candidate) > MaxICECandidateSize {
			return invalid("ICE candidate size must be between 1 and %d bytes", MaxICECandidateSize)
		}
	}

	return nil
}

func (v Validator) ValidateControl(message ControlMessage) error {
	if err := v.validateCommon(message.Version, message.ID, message.SentAt); err != nil {
		return err
	}
	if !message.Type.IsKnown() {
		return invalid("unknown control type %q", message.Type)
	}

	switch message.Type {
	case ControlPeerHello:
		var payload PeerHelloPayload
		if err := requiredPayload(message.Payload, &payload); err != nil || !payload.Capabilities.TextChat {
			return invalid("peer.hello must advertise text_chat capability")
		}
	case ControlChatMessage:
		var payload ChatPayload
		if err := requiredPayload(message.Payload, &payload); err != nil {
			return invalid("chat.message requires a text payload")
		}
		if payload.Text == "" {
			return invalid("chat text must not be empty")
		}
		if !utf8.ValidString(payload.Text) || len(payload.Text) > MaxChatTextSize {
			return invalid("chat text must be valid UTF-8 and at most %d bytes", MaxChatTextSize)
		}
	case ControlSessionHangup:
		if len(message.Payload) != 0 && !bytes.Equal(bytes.TrimSpace(message.Payload), []byte("{}")) {
			return invalid("session.hangup payload must be empty")
		}
	}

	return nil
}

func (v Validator) validateCommon(version int, id string, timestamp time.Time) error {
	if version != ProtocolVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, ProtocolVersion)
	}
	if !validUUID(id) {
		return invalid("invalid message ID %q", id)
	}
	if timestamp.IsZero() {
		return invalid("timestamp is required")
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	maximumAge := v.MaximumAge
	if maximumAge == 0 {
		maximumAge = 24 * time.Hour
	}
	maximumFuture := v.MaximumFuture
	if maximumFuture == 0 {
		maximumFuture = 5 * time.Minute
	}
	if timestamp.Before(now.Add(-maximumAge)) || timestamp.After(now.Add(maximumFuture)) {
		return invalid("timestamp is outside the accepted window")
	}
	return nil
}

func ValidUsername(username string) bool {
	return len(username) >= MinUsernameLength &&
		len(username) <= MaxUsernameLength &&
		usernamePattern.MatchString(username)
}

func validUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

func requiredPayload(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("payload is required")
	}
	return json.Unmarshal(raw, destination)
}

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(format, arguments...))
}
