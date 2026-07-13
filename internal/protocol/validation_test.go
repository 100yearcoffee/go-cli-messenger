package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func testValidator() Validator {
	validator := NewValidator()
	validator.Now = func() time.Time { return testNow }
	return validator
}

func validSignal(messageType SignalType) SignalMessage {
	return SignalMessage{
		Version:   ProtocolVersion,
		ID:        "0191bda8-41c0-7cb8-a2fd-71ab5d784f53",
		Type:      messageType,
		Timestamp: testNow,
		CallID:    "0191bda8-a022-765a-9af0-bfe74b1190f1",
		From:      "alice",
		To:        "bob",
	}
}

func validControl(messageType ControlType) ControlMessage {
	return ControlMessage{
		Version: ProtocolVersion,
		ID:      "0191bdb0-c0d5-7588-b687-8f5ed8016c8f",
		Type:    messageType,
		SentAt:  testNow,
	}
}

func TestValidateSignal(t *testing.T) {
	t.Parallel()

	offer := validSignal(SignalWebRTCOffer)
	offer.Payload = json.RawMessage(`{"sdp":"v=0\\r\\n"}`)
	if err := testValidator().ValidateSignal(offer); err != nil {
		t.Fatalf("valid offer rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*SignalMessage)
	}{
		{"version", func(message *SignalMessage) { message.Version = 2 }},
		{"message ID", func(message *SignalMessage) { message.ID = "not-a-uuid" }},
		{"unknown type", func(message *SignalMessage) { message.Type = "future.message" }},
		{"old timestamp", func(message *SignalMessage) { message.Timestamp = testNow.Add(-25 * time.Hour) }},
		{"invalid sender", func(message *SignalMessage) { message.From = "Alice!" }},
		{"missing recipient", func(message *SignalMessage) { message.To = "" }},
		{"missing call ID", func(message *SignalMessage) { message.CallID = "" }},
		{"missing SDP", func(message *SignalMessage) { message.Payload = nil }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			message := offer
			test.mutate(&message)
			if err := testValidator().ValidateSignal(message); err == nil {
				t.Fatal("invalid message accepted")
			}
		})
	}
}

func TestValidateSignalTypes(t *testing.T) {
	t.Parallel()

	hello := validSignal(SignalSessionHello)
	hello.CallID = ""
	hello.To = ""
	if err := testValidator().ValidateSignal(hello); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}

	ice := validSignal(SignalWebRTCICE)
	ice.Payload = json.RawMessage(`{"candidate":"candidate:1 1 udp 1 127.0.0.1 1234 typ host"}`)
	if err := testValidator().ValidateSignal(ice); err != nil {
		t.Fatalf("valid ICE rejected: %v", err)
	}
	ice.Payload = json.RawMessage(`{"candidate":""}`)
	if err := testValidator().ValidateSignal(ice); err == nil {
		t.Fatal("empty ICE candidate accepted")
	}
}

func TestDecodeSignalLimitsAndTrailingJSON(t *testing.T) {
	t.Parallel()

	message := validSignal(SignalCallInvite)
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSignal(encoded, testValidator()); err != nil {
		t.Fatalf("valid encoded signal rejected: %v", err)
	}
	if _, err := DecodeSignal(append(encoded, []byte(` {}`)...), testValidator()); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("trailing JSON error = %v, want ErrInvalidMessage", err)
	}
	oversized := []byte(`{"type":"call.invite","padding":"` + strings.Repeat("x", MaxSignalMessageSize) + `"}`)
	if _, err := DecodeSignal(oversized, testValidator()); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize error = %v, want ErrMessageTooLarge", err)
	}
}

func TestValidateControl(t *testing.T) {
	t.Parallel()

	chat := validControl(ControlChatMessage)
	chat.Payload = json.RawMessage(`{"text":"hello, 世界"}`)
	if err := testValidator().ValidateControl(chat); err != nil {
		t.Fatalf("valid chat rejected: %v", err)
	}

	tooLong := chat
	payload, err := json.Marshal(ChatPayload{Text: strings.Repeat("x", MaxChatTextSize+1)})
	if err != nil {
		t.Fatal(err)
	}
	tooLong.Payload = payload
	if err := testValidator().ValidateControl(tooLong); err == nil {
		t.Fatal("oversized chat accepted")
	}

	hello := validControl(ControlPeerHello)
	hello.Payload = json.RawMessage(`{"capabilities":{"text_chat":true,"ascii_video":false,"audio":false}}`)
	if err := testValidator().ValidateControl(hello); err != nil {
		t.Fatalf("valid peer hello rejected: %v", err)
	}

	hangup := validControl(ControlSessionHangup)
	if err := testValidator().ValidateControl(hangup); err != nil {
		t.Fatalf("valid hangup rejected: %v", err)
	}
	hangup.Payload = json.RawMessage(`{"reason":"bye"}`)
	if err := testValidator().ValidateControl(hangup); err == nil {
		t.Fatal("hangup with unexpected payload accepted")
	}
}

func TestValidUsername(t *testing.T) {
	t.Parallel()

	for _, username := range []string{"alice", "bob_2", "user-name"} {
		if !ValidUsername(username) {
			t.Errorf("ValidUsername(%q) = false", username)
		}
	}
	for _, username := range []string{"ab", "Alice", "bad name", strings.Repeat("a", 33)} {
		if ValidUsername(username) {
			t.Errorf("ValidUsername(%q) = true", username)
		}
	}
}
