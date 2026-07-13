package media

import (
	"context"
	"time"

	"github.com/pion/rtp"
)

type VideoConfig struct {
	Device  string
	Columns int
	Rows    int
	FPS     int
}

type VideoFrame struct {
	Pixels    []byte
	Width     int
	Height    int
	Timestamp time.Time
}

type Camera interface {
	Open(context.Context, VideoConfig) (<-chan VideoFrame, error)
	Close() error
}

type AudioConfig struct {
	Device  string
	Bitrate int
}

type AudioInput interface {
	Start(context.Context, AudioConfig) (<-chan *rtp.Packet, error)
	Close() error
}

type AudioOutput interface {
	Start(context.Context, AudioConfig, <-chan *rtp.Packet) error
	Close() error
}
