//go:build darwin

package gstreamer

func cameraSource(device string) []string {
	source := []string{"avfvideosrc"}
	if device != "" {
		source = append(source, "device-index="+device)
	}
	return source
}
