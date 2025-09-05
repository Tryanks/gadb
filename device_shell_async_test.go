package gadb

import (
	"io"
	"testing"
	"time"
)

func TestDevice_RunShellCommandAsync(t *testing.T) {
	adbClient, err := NewClient()
	if err != nil {
		t.Skip("adb server not available: ", err)
		return
	}

	devices, err := adbClient.DeviceList()
	if err != nil || len(devices) == 0 {
		t.Skip("no devices connected")
		return
	}

	dev := devices[0]
	SetDebug(true)

	sh, err := dev.RunShellCommandAsync("logcat")
	if err != nil {
		t.Fatal(err)
	}
	if sh == nil || sh.Reader == nil {
		t.Fatal("shell or reader is nil")
	}

	go func() { _, _ = io.Copy(io.Discard, sh.Reader) }()
	// Let it run briefly, then close (simulate Ctrl+C)
	time.Sleep(500 * time.Millisecond)
	if err := sh.Close(); err != nil {
		t.Fatal(err)
	}
}
