package asciivideo

import (
	"errors"
	"testing"
	"time"
)

func TestKeyframeDeltaAndLossRecovery(t *testing.T) {
	t.Parallel()
	encoder, err := NewEncoder(4, 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decoder := new(Decoder)
	now := time.Unix(100, 0)
	key, err := encoder.Encode([]byte("abcdefgh"), now)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := decoder.Decode(key)
	if err != nil || string(frame.Cells) != "abcdefgh" || frame.Type != FrameKey {
		t.Fatalf("keyframe = %#v, %v", frame, err)
	}
	delta, err := encoder.Encode([]byte("abcXefgh"), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	frame, err = decoder.Decode(delta)
	if err != nil || string(frame.Cells) != "abcXefgh" || frame.Type != FrameDelta {
		t.Fatalf("delta = %#v, %v", frame, err)
	}
	missing, _ := encoder.Encode([]byte("abcXYfgh"), now.Add(2*time.Second))
	afterMissing, _ := encoder.Encode([]byte("abcXYZgh"), now.Add(3*time.Second))
	_ = missing
	if _, err := decoder.Decode(afterMissing); !errors.Is(err, ErrNeedsKeyframe) {
		t.Fatalf("lost delta error = %v, want ErrNeedsKeyframe", err)
	}
	encoder.ForceKeyframe()
	recovery, _ := encoder.Encode([]byte("abcXYZgh"), now.Add(4*time.Second))
	if frame, err = decoder.Decode(recovery); err != nil || string(frame.Cells) != "abcXYZgh" {
		t.Fatalf("recovery = %#v, %v", frame, err)
	}
}
