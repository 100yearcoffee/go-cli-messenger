package asciivideo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	Version           = 1
	CharsetMonochrome = 1
	ColorMonochrome   = 0
	MaxColumns        = 200
	MaxRows           = 80
	MaxEncodedSize    = 64 << 10
	headerSize        = 26
)

var (
	ErrInvalidFrame  = errors.New("invalid ASCII video frame")
	ErrNeedsKeyframe = errors.New("ASCII video keyframe required")
	magic            = [2]byte{'T', 'V'}
)

type FrameType uint8

const (
	FrameKey FrameType = iota + 1
	FrameDelta
)

type Frame struct {
	Type      FrameType
	Sequence  uint32
	Timestamp time.Time
	Columns   int
	Rows      int
	Cells     []byte
}

// Encoder emits periodic keyframes and compact runs for changed cells.
type Encoder struct {
	columns      int
	rows         int
	keyInterval  time.Duration
	previous     []byte
	sequence     uint32
	lastKeyframe time.Time
	forceKey     bool
}

func NewEncoder(columns, rows int, keyInterval time.Duration) (*Encoder, error) {
	if err := validateDimensions(columns, rows); err != nil {
		return nil, err
	}
	if keyInterval <= 0 {
		keyInterval = 2 * time.Second
	}
	return &Encoder{columns: columns, rows: rows, keyInterval: keyInterval, forceKey: true}, nil
}

func (e *Encoder) ForceKeyframe() { e.forceKey = true }

func (e *Encoder) Encode(cells []byte, timestamp time.Time) ([]byte, error) {
	if len(cells) != e.columns*e.rows {
		return nil, fmt.Errorf("%w: got %d cells, want %d", ErrInvalidFrame, len(cells), e.columns*e.rows)
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	for _, cell := range cells {
		if cell < 0x20 || cell > 0x7e {
			return nil, fmt.Errorf("%w: unsafe cell byte 0x%02x", ErrInvalidFrame, cell)
		}
	}

	e.sequence++
	frameType := FrameDelta
	payload := encodeDelta(e.previous, cells)
	if e.forceKey || len(e.previous) == 0 || timestamp.Sub(e.lastKeyframe) >= e.keyInterval || len(payload) >= len(cells) {
		frameType = FrameKey
		payload = append([]byte(nil), cells...)
		e.lastKeyframe = timestamp
		e.forceKey = false
	}
	encoded, err := marshal(frameType, e.sequence, timestamp, e.columns, e.rows, payload)
	if err != nil {
		return nil, err
	}
	e.previous = append(e.previous[:0], cells...)
	return encoded, nil
}

// Decoder keeps the last complete frame and rejects deltas after packet loss.
type Decoder struct {
	columns  int
	rows     int
	sequence uint32
	cells    []byte
}

func (d *Decoder) Decode(data []byte) (Frame, error) {
	frameType, sequence, timestamp, columns, rows, payload, err := unmarshal(data)
	if err != nil {
		return Frame{}, err
	}
	cellCount := columns * rows
	switch frameType {
	case FrameKey:
		if len(payload) != cellCount {
			return Frame{}, fmt.Errorf("%w: keyframe payload has %d cells, want %d", ErrInvalidFrame, len(payload), cellCount)
		}
		d.cells = append(d.cells[:0], payload...)
	case FrameDelta:
		if len(d.cells) != cellCount || d.columns != columns || d.rows != rows || sequence != d.sequence+1 {
			return Frame{}, ErrNeedsKeyframe
		}
		if err := applyDelta(d.cells, payload); err != nil {
			return Frame{}, err
		}
	default:
		return Frame{}, fmt.Errorf("%w: unknown frame type %d", ErrInvalidFrame, frameType)
	}
	for _, cell := range d.cells {
		if cell < 0x20 || cell > 0x7e {
			return Frame{}, fmt.Errorf("%w: unsafe cell byte 0x%02x", ErrInvalidFrame, cell)
		}
	}
	d.columns, d.rows, d.sequence = columns, rows, sequence
	return Frame{Type: frameType, Sequence: sequence, Timestamp: timestamp, Columns: columns, Rows: rows, Cells: append([]byte(nil), d.cells...)}, nil
}

func marshal(frameType FrameType, sequence uint32, timestamp time.Time, columns, rows int, payload []byte) ([]byte, error) {
	if err := validateDimensions(columns, rows); err != nil {
		return nil, err
	}
	if headerSize+len(payload) > MaxEncodedSize {
		return nil, fmt.Errorf("%w: encoded frame is too large", ErrInvalidFrame)
	}
	result := make([]byte, headerSize+len(payload))
	copy(result[:2], magic[:])
	result[2] = Version
	result[3] = byte(frameType)
	binary.BigEndian.PutUint32(result[4:8], sequence)
	binary.BigEndian.PutUint64(result[8:16], uint64(timestamp.UnixMilli()))
	binary.BigEndian.PutUint16(result[16:18], uint16(columns))
	binary.BigEndian.PutUint16(result[18:20], uint16(rows))
	result[20] = CharsetMonochrome
	result[21] = ColorMonochrome
	binary.BigEndian.PutUint32(result[22:26], uint32(len(payload)))
	copy(result[headerSize:], payload)
	return result, nil
}

func unmarshal(data []byte) (FrameType, uint32, time.Time, int, int, []byte, error) {
	if len(data) < headerSize || len(data) > MaxEncodedSize || data[0] != magic[0] || data[1] != magic[1] {
		return 0, 0, time.Time{}, 0, 0, nil, ErrInvalidFrame
	}
	if data[2] != Version {
		return 0, 0, time.Time{}, 0, 0, nil, fmt.Errorf("%w: version %d", ErrInvalidFrame, data[2])
	}
	columns, rows := int(binary.BigEndian.Uint16(data[16:18])), int(binary.BigEndian.Uint16(data[18:20]))
	if err := validateDimensions(columns, rows); err != nil {
		return 0, 0, time.Time{}, 0, 0, nil, err
	}
	if data[20] != CharsetMonochrome || data[21] != ColorMonochrome {
		return 0, 0, time.Time{}, 0, 0, nil, fmt.Errorf("%w: unsupported profile", ErrInvalidFrame)
	}
	payloadSize := int(binary.BigEndian.Uint32(data[22:26]))
	if payloadSize != len(data)-headerSize {
		return 0, 0, time.Time{}, 0, 0, nil, fmt.Errorf("%w: payload size mismatch", ErrInvalidFrame)
	}
	timestamp := time.UnixMilli(int64(binary.BigEndian.Uint64(data[8:16])))
	return FrameType(data[3]), binary.BigEndian.Uint32(data[4:8]), timestamp, columns, rows, data[headerSize:], nil
}

func encodeDelta(previous, current []byte) []byte {
	if len(previous) != len(current) {
		return nil
	}
	result := make([]byte, 0)
	for index := 0; index < len(current); {
		if current[index] == previous[index] {
			index++
			continue
		}
		start := index
		for index < len(current) && current[index] != previous[index] && index-start < 0xffff {
			index++
		}
		runLength := index - start
		entry := make([]byte, 6+runLength)
		binary.BigEndian.PutUint32(entry[:4], uint32(start))
		binary.BigEndian.PutUint16(entry[4:6], uint16(runLength))
		copy(entry[6:], current[start:index])
		result = append(result, entry...)
	}
	return result
}

func applyDelta(cells, payload []byte) error {
	for len(payload) > 0 {
		if len(payload) < 6 {
			return fmt.Errorf("%w: truncated delta entry", ErrInvalidFrame)
		}
		start := int(binary.BigEndian.Uint32(payload[:4]))
		runLength := int(binary.BigEndian.Uint16(payload[4:6]))
		payload = payload[6:]
		if runLength == 0 || runLength > len(payload) || start > len(cells)-runLength {
			return fmt.Errorf("%w: invalid delta run", ErrInvalidFrame)
		}
		copy(cells[start:start+runLength], payload[:runLength])
		payload = payload[runLength:]
	}
	return nil
}

func validateDimensions(columns, rows int) error {
	if columns < 1 || columns > MaxColumns || rows < 1 || rows > MaxRows {
		return fmt.Errorf("%w: dimensions %dx%d exceed %dx%d", ErrInvalidFrame, columns, rows, MaxColumns, MaxRows)
	}
	return nil
}
