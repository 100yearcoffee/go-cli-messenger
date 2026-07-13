package gstreamer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"

	"termcall/internal/client/media"
)

const (
	opusPayloadType  = 111
	maxRTPPacketSize = 8192
)

type AudioInput struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func (input *AudioInput) Start(ctx context.Context, config media.AudioConfig) (<-chan *rtp.Packet, error) {
	if err := requireAudioRuntime(); err != nil {
		return nil, err
	}
	bitrate := config.Bitrate
	if bitrate == 0 {
		bitrate = 32000
	}
	if bitrate < 24000 || bitrate > 40000 {
		return nil, errors.New("Opus bitrate must be between 24000 and 40000 bits per second")
	}
	source := []string{"autoaudiosrc"}
	if config.Device != "" {
		source = []string{"pulsesrc", "device=" + config.Device}
	}
	arguments := append([]string{"-q"}, source...)
	arguments = append(arguments,
		"!", "audioconvert", "!", "audioresample",
		"!", "audio/x-raw,format=S16LE,rate=48000,channels=1",
		"!", "opusenc", fmt.Sprintf("bitrate=%d", bitrate), "frame-size=20", "audio-type=voice", "inband-fec=true", "packet-loss-percentage=5",
		"!", "rtpopuspay", "pt=111", "!", "rtpstreampay", "!", "fdsink", "fd=1", "sync=false",
	)
	captureContext, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(captureContext, "gst-launch-1.0", arguments...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open GStreamer audio output: %w", err)
	}
	var standardError bytes.Buffer
	command.Stderr = &limitedWriter{destination: &standardError, remaining: 4096}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start GStreamer microphone pipeline: %w", err)
	}

	input.mu.Lock()
	if input.done != nil {
		input.mu.Unlock()
		cancel()
		_ = command.Wait()
		return nil, errors.New("audio input is already started")
	}
	input.cancel, input.done = cancel, make(chan struct{})
	done := input.done
	input.mu.Unlock()

	packets := make(chan *rtp.Packet, 10)
	go func() {
		defer close(done)
		defer close(packets)
		reader := bufio.NewReaderSize(stdout, 16<<10)
		for {
			packet, err := readRTPStreamPacket(reader)
			if err != nil {
				break
			}
			select {
			case packets <- packet:
				continue
			default:
			}
			select {
			case <-packets:
			default:
			}
			select {
			case packets <- packet:
			case <-captureContext.Done():
				_ = command.Wait()
				return
			}
		}
		_ = command.Wait()
	}()
	return packets, nil
}

func (input *AudioInput) Close() error {
	input.closeOnce.Do(func() {
		input.mu.Lock()
		cancel, done := input.cancel, input.done
		input.mu.Unlock()
		stopProcess(cancel, done)
	})
	return nil
}

type AudioOutput struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func (output *AudioOutput) Start(ctx context.Context, config media.AudioConfig, packets <-chan *rtp.Packet) error {
	if packets == nil {
		return errors.New("audio packet source is required")
	}
	if err := requireAudioRuntime(); err != nil {
		return err
	}
	sink := []string{"autoaudiosink", "sync=false"}
	if config.Device != "" {
		sink = []string{"pulsesink", "device=" + config.Device, "sync=false"}
	}
	arguments := []string{
		"-q", "fdsrc", "fd=0",
		"!", "application/x-rtp-stream,media=audio,encoding-name=OPUS,payload=111,clock-rate=48000",
		"!", "rtpstreamdepay", "!", "rtpopusdepay", "!", "opusdec", "!", "audioconvert", "!", "audioresample", "!",
	}
	arguments = append(arguments, sink...)
	playbackContext, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(playbackContext, "gst-launch-1.0", arguments...)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("open GStreamer audio input: %w", err)
	}
	var standardError bytes.Buffer
	command.Stderr = &limitedWriter{destination: &standardError, remaining: 4096}
	if err := command.Start(); err != nil {
		cancel()
		return fmt.Errorf("start GStreamer speaker pipeline: %w", err)
	}

	output.mu.Lock()
	if output.done != nil {
		output.mu.Unlock()
		cancel()
		_ = command.Wait()
		return errors.New("audio output is already started")
	}
	output.cancel, output.done = cancel, make(chan struct{})
	done := output.done
	output.mu.Unlock()

	go func() {
		defer close(done)
		defer stdin.Close()
		for {
			select {
			case <-playbackContext.Done():
				_ = command.Wait()
				return
			case packet, ok := <-packets:
				if !ok {
					_ = stdin.Close()
					_ = command.Wait()
					return
				}
				if packet == nil {
					continue
				}
				copy := *packet
				copy.PayloadType = opusPayloadType
				if err := writeRTPStreamPacket(stdin, &copy); err != nil {
					_ = command.Wait()
					return
				}
			}
		}
	}()
	return nil
}

func (output *AudioOutput) Close() error {
	output.closeOnce.Do(func() {
		output.mu.Lock()
		cancel, done := output.cancel, output.done
		output.mu.Unlock()
		stopProcess(cancel, done)
	})
	return nil
}

func ListAudioDevices(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gst-device-monitor-1.0"); err != nil {
		return "", errors.New("gst-device-monitor-1.0 was not found")
	}
	monitorContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(monitorContext, "gst-device-monitor-1.0", "Audio/Source", "Audio/Sink").CombinedOutput()
	text := strings.TrimSpace(string(output))
	if monitorContext.Err() != nil && text != "" {
		return text, nil
	}
	if err != nil {
		return text, fmt.Errorf("enumerate GStreamer audio devices: %w", err)
	}
	if text == "" {
		text = "No GStreamer audio devices detected."
	}
	return text, nil
}

func AudioRuntimeStatus() error { return requireAudioRuntime() }

func requireAudioRuntime() error {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return errors.New("GStreamer is required for audio (gst-launch-1.0 was not found)")
	}
	if _, err := exec.LookPath("gst-inspect-1.0"); err != nil {
		return errors.New("GStreamer plugin inspector was not found")
	}
	required := []string{"opusenc", "opusdec", "rtpopuspay", "rtpopusdepay", "rtpstreampay", "rtpstreamdepay", "autoaudiosrc", "autoaudiosink", "pulsesrc", "pulsesink"}
	arguments := append([]string(nil), required...)
	if err := exec.Command("gst-inspect-1.0", arguments...).Run(); err != nil {
		return fmt.Errorf("required GStreamer audio plugins are unavailable: %w", err)
	}
	selfTest := []string{
		"-q", "audiotestsrc", "num-buffers=5", "!", "audioconvert", "!", "audioresample",
		"!", "audio/x-raw,format=S16LE,rate=48000,channels=1", "!", "opusenc", "bitrate=32000", "frame-size=20",
		"!", "rtpopuspay", "pt=111", "!", "rtpstreampay", "!", "rtpstreamdepay",
		"!", "rtpopusdepay", "!", "opusdec", "!", "fakesink",
	}
	if err := exec.Command("gst-launch-1.0", selfTest...).Run(); err != nil {
		return fmt.Errorf("GStreamer Opus self-test failed: %w", err)
	}
	return nil
}

func readRTPStreamPacket(reader io.Reader) (*rtp.Packet, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size < 12 || size > maxRTPPacketSize {
		return nil, fmt.Errorf("invalid framed RTP packet size %d", size)
	}
	encoded := make([]byte, size)
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return nil, err
	}
	packet := new(rtp.Packet)
	if err := packet.Unmarshal(encoded); err != nil {
		return nil, fmt.Errorf("decode microphone RTP: %w", err)
	}
	return packet, nil
}

func writeRTPStreamPacket(writer io.Writer, packet *rtp.Packet) error {
	encoded, err := packet.Marshal()
	if err != nil {
		return fmt.Errorf("encode speaker RTP: %w", err)
	}
	if len(encoded) > maxRTPPacketSize {
		return fmt.Errorf("speaker RTP packet exceeds %d bytes", maxRTPPacketSize)
	}
	framed := make([]byte, 2+len(encoded))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(encoded)))
	copy(framed[2:], encoded)
	if _, err := writer.Write(framed); err != nil {
		return fmt.Errorf("write speaker RTP: %w", err)
	}
	return nil
}

func stopProcess(cancel context.CancelFunc, done <-chan struct{}) {
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}
