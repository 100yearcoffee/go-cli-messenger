package gstreamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"termcall/internal/client/media"
)

// Camera captures fixed-size grayscale frames from a Linux V4L2 device using
// the system GStreamer runtime. Keeping the process adapter here isolates the
// native media dependency from the rest of the client.
type Camera struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	command   *exec.Cmd
	done      chan struct{}
	closeOnce sync.Once
}

func (c *Camera) Open(ctx context.Context, config media.VideoConfig) (<-chan media.VideoFrame, error) {
	if config.Columns < 1 || config.Rows < 1 || config.FPS < 1 {
		return nil, errors.New("invalid camera dimensions or frame rate")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return nil, errors.New("GStreamer is required for camera capture (gst-launch-1.0 was not found)")
	}
	device := config.Device
	if device == "" {
		device = "/dev/video0"
	}
	width, height := config.Columns, config.Rows*2
	arguments := []string{
		"-q", "v4l2src", "device=" + device,
		"!", "videoconvert", "!", "videoscale",
		"!", fmt.Sprintf("video/x-raw,format=GRAY8,width=%d,height=%d,framerate=%d/1", width, height, config.FPS),
		"!", "fdsink", "fd=1", "sync=false",
	}
	captureContext, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(captureContext, "gst-launch-1.0", arguments...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open GStreamer video output: %w", err)
	}
	var standardError bytes.Buffer
	command.Stderr = &limitedWriter{destination: &standardError, remaining: 4096}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start GStreamer camera pipeline: %w", err)
	}

	c.mu.Lock()
	if c.command != nil {
		c.mu.Unlock()
		cancel()
		_ = command.Wait()
		return nil, errors.New("camera is already open")
	}
	c.cancel, c.command, c.done = cancel, command, make(chan struct{})
	done := c.done
	c.mu.Unlock()

	frames := make(chan media.VideoFrame, 1)
	go func() {
		defer close(done)
		defer close(frames)
		frameSize := width * height
		for {
			pixels := make([]byte, frameSize)
			if _, err := io.ReadFull(stdout, pixels); err != nil {
				break
			}
			frame := media.VideoFrame{Pixels: pixels, Width: width, Height: height, Timestamp: time.Now()}
			select {
			case frames <- frame:
			default:
				// Capture never waits for a slow encoder: replace the stale frame.
				select {
				case <-frames:
				default:
				}
				select {
				case frames <- frame:
				default:
				}
			}
		}
		_ = command.Wait()
	}()
	return frames, nil
}

func (c *Camera) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		cancel, done := c.cancel, c.done
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})
	return nil
}

type limitedWriter struct {
	destination io.Writer
	remaining   int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if len(data) > w.remaining {
		data = data[:w.remaining]
	}
	if len(data) > 0 {
		if _, err := w.destination.Write(data); err != nil {
			return 0, err
		}
		w.remaining -= len(data)
	}
	return originalLength, nil
}
