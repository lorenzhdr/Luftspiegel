package daemon

import (
	"strings"
	"testing"
)

func TestNewRejectsUnknownHWAccel(t *testing.T) {
	daemon, err := New(Config{HWAccel: "bogus"})
	if err == nil {
		if daemon != nil {
			t.Fatal("New returned a daemon for an unknown hwaccel value")
		}
		t.Fatal("New accepted an unknown hwaccel value")
	}
	if !strings.Contains(err.Error(), `unknown H.264 encoder "bogus"`) {
		t.Fatalf("New error = %q", err)
	}
}

func TestNewRejectsUnknownRateControl(t *testing.T) {
	daemon, err := New(Config{RateControl: "bogus"})
	if err == nil {
		if daemon != nil {
			t.Fatal("New returned a daemon for an unknown rate control value")
		}
		t.Fatal("New accepted an unknown rate control value")
	}
	if !strings.Contains(err.Error(), `unknown rate control "bogus"`) {
		t.Fatalf("New error = %q", err)
	}
}
