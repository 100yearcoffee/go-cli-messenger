package diagnostics

import (
	"context"
	"fmt"
	"io"

	"termcall/internal/client/media/gstreamer"
)

func Devices(ctx context.Context, output io.Writer) error {
	devices, err := gstreamer.ListAudioDevices(ctx)
	if devices != "" {
		fmt.Fprintln(output, devices)
	}
	return err
}

func Doctor(ctx context.Context, output io.Writer) error {
	if err := gstreamer.AudioRuntimeStatus(); err != nil {
		fmt.Fprintf(output, "[fail] GStreamer Opus audio: %v\n", err)
		return err
	}
	fmt.Fprintln(output, "[ok] GStreamer Opus capture and playback plugins")
	devices, err := gstreamer.ListAudioDevices(ctx)
	if err != nil {
		fmt.Fprintf(output, "[fail] audio device discovery: %v\n", err)
		return err
	}
	fmt.Fprintln(output, "[ok] audio device discovery")
	fmt.Fprintln(output, devices)
	return nil
}
