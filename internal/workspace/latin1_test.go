package workspace

import "testing"

func TestLatin1RoundTripLossless(t *testing.T) {
	// Every byte value survives Decode→BytesFromView unchanged — including a lone
	// 0x97 (invalid UTF-8) and a run of valid UTF-8.
	for _, in := range [][]byte{
		{0x61, 0x97, 0x62},       // the em-dash bug: a...\x97...b
		[]byte("plain ascii"),    // ascii
		{0xE2, 0x80, 0x94},       // a real UTF-8 em-dash, byte-preserved
		{0x00, 0xFF, 0x80, 0x7F}, // boundaries
	} {
		if got := BytesFromView(Latin1Decode(in)); string(got) != string(in) {
			t.Errorf("round-trip changed bytes: %v -> %v", in, got)
		}
	}
}

func TestLatin1RepairEmDash(t *testing.T) {
	// Repairing the bug: replace the lone 0x97 (view rune U+0097) with a real
	// em-dash (U+2014); the result must be valid UTF-8 with the correct 3 bytes.
	view := Latin1Decode([]byte{0x61, 0x97, 0x62}) // "ab"
	fixed := view[:1] + "—" + view[len(view)-1:]
	got := BytesFromView(fixed)
	if string(got) != "a—b" || !ValidUTF8(got) {
		t.Errorf("repair = %v (%q), want valid UTF-8 a—b", got, got)
	}
}
