package airplay

import (
	"strings"
	"testing"
)

// TestValidateHWAccel exercises ValidateHWAccel directly, which lives in the
// platform-agnostic capture.go. It has no build tag so it runs on every
// platform's `go test ./...`, unlike the rest of the GStreamer-specific
// capture tests in capture_test.go (linux-only).
func TestValidateHWAccel(t *testing.T) {
	for _, method := range []string{"", "auto", "nvenc", "vaapi", "openh264", "none"} {
		if err := ValidateHWAccel(method); err != nil {
			t.Errorf("ValidateHWAccel(%q): %v", method, err)
		}
	}
	for _, method := range []string{"x264", "OPENH264", "bogus", " auto"} {
		err := ValidateHWAccel(method)
		if err == nil {
			t.Errorf("ValidateHWAccel(%q) succeeded", method)
		} else if !strings.Contains(err.Error(), method) {
			t.Errorf("ValidateHWAccel(%q) error %q does not name the invalid value", method, err)
		}
	}
}
