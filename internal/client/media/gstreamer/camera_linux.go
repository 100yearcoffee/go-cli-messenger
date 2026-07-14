//go:build linux

package gstreamer

func cameraSource(device string) []string {
	if device == "" {
		device = "/dev/video0"
	}
	return []string{"v4l2src", "device=" + device}
}
