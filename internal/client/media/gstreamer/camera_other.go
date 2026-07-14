//go:build !linux && !darwin

package gstreamer

func cameraSource(string) []string { return []string{"autovideosrc"} }
