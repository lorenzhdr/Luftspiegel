package airplay

import (
	"bytes"
	"context"
	"testing"
)

// nal builds an Annex-B NAL unit (4-byte start code + a 1-byte header with
// the given type, forbidden_zero_bit=0, nal_ref_idc=0 + a payload byte) for
// use in test fixtures.
func nal(nalUnitType byte, payload ...byte) []byte {
	b := []byte{0, 0, 0, 1, nalUnitType & 0x1f}
	return append(b, payload...)
}

func TestSplitAnnexBAccessUnits(t *testing.T) {
	sps := nal(7, 0xAA)
	pps := nal(8, 0xBB)
	idr := nal(5, 0x01)
	p1 := nal(1, 0x02)
	p2 := nal(1, 0x03)
	aud := nal(9, 0xF0)

	var data []byte
	data = append(data, sps...)
	data = append(data, pps...)
	data = append(data, idr...) // AU 1: SPS+PPS+IDR
	data = append(data, aud...)
	data = append(data, p1...) // AU 2: AUD+P
	data = append(data, p2...) // AU 3: P only

	units := splitAnnexBAccessUnits(data)
	if len(units) != 3 {
		t.Fatalf("got %d access units, want 3: %v", len(units), units)
	}

	var want1 []byte
	want1 = append(want1, sps...)
	want1 = append(want1, pps...)
	want1 = append(want1, idr...)
	if !bytes.Equal(units[0], want1) {
		t.Errorf("AU 1 = %x, want %x", units[0], want1)
	}

	var want2 []byte
	want2 = append(want2, aud...)
	want2 = append(want2, p1...)
	if !bytes.Equal(units[1], want2) {
		t.Errorf("AU 2 = %x, want %x", units[1], want2)
	}

	if !bytes.Equal(units[2], p2) {
		t.Errorf("AU 3 = %x, want %x", units[2], p2)
	}
}

func TestSplitAnnexBAccessUnitsDropsTrailingNonVCL(t *testing.T) {
	sps := nal(7, 0xAA)
	idr := nal(5, 0x01)
	trailingSEI := nal(6, 0xEE) // no slice follows — should be dropped, not hang

	var data []byte
	data = append(data, sps...)
	data = append(data, idr...)
	data = append(data, trailingSEI...)

	units := splitAnnexBAccessUnits(data)
	if len(units) != 1 {
		t.Fatalf("got %d access units, want 1: %v", len(units), units)
	}
	var want []byte
	want = append(want, sps...)
	want = append(want, idr...)
	if !bytes.Equal(units[0], want) {
		t.Errorf("AU = %x, want %x", units[0], want)
	}
}

func TestSplitAnnexBAccessUnitsEmpty(t *testing.T) {
	if got := splitAnnexBAccessUnits(nil); got != nil {
		t.Errorf("splitAnnexBAccessUnits(nil) = %v, want nil", got)
	}
	if got := splitAnnexBAccessUnits([]byte{1, 2, 3}); got != nil {
		t.Errorf("splitAnnexBAccessUnits(no start code) = %v, want nil", got)
	}
}

func TestStartStubCaptureRejectsMissingFile(t *testing.T) {
	_, err := StartStubCapture(context.Background(), CaptureConfig{StubFile: "does-not-exist.h264"})
	if err == nil {
		t.Fatal("StartStubCapture with a missing file did not return an error")
	}
}
