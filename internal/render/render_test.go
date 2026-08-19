package render

import "testing"

func names(js []Journey) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.Depart
	}
	return out
}

func TestDropBrokenRemovesUnmakeableJourneys(t *testing.T) {
	in := []Journey{
		{Depart: "13:08"},
		{Depart: "13:23", Warning: "This connection no longer works: ..."},
		{Depart: "13:38"},
	}
	got := DropBroken(in)
	if len(got) != 2 || got[0].Depart != "13:08" || got[1].Depart != "13:38" {
		t.Fatalf("got %v, want the two workable journeys", names(got))
	}
}

// Every option broken is the one case where they must still be shown: an empty
// list reads as "nothing runs", and a passenger already on the first leg needs
// to know the connection went.
func TestDropBrokenKeepsThemWhenAllAreBroken(t *testing.T) {
	in := []Journey{
		{Depart: "13:23", Warning: "broken"},
		{Depart: "13:38", Warning: "broken"},
	}
	if got := DropBroken(in); len(got) != 2 {
		t.Fatalf("got %d journeys, want both: an empty list is a different answer", len(got))
	}
}

func TestDropBrokenOnEmptyInput(t *testing.T) {
	if got := DropBroken(nil); len(got) != 0 {
		t.Fatalf("got %d, want 0", len(got))
	}
}

func TestParseModeListNarrowsWhatGetsLoaded(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", 0},           // nothing asked for; callers read nil as everything
		{"train", 2},      // metro + regional
		{"train,tram", 3}, //  + metro tram
		{"tram,tram", 1},  // deduplicated
		{"train,nonsense", 2},
		{" Train , Tram ", 3}, // whitespace and case
		{"bus", 3},
	} {
		got, _ := ParseModeList(c.in)
		if len(got) != c.want {
			t.Errorf("ParseModeList(%q) = %v, want %d modes", c.in, got, c.want)
		}
	}
}

// A typo must be reported, not ignored: an empty list means "everything", so
// "trian" would download and load every mode instead of the trains asked for.
func TestParseModeListReportsTypos(t *testing.T) {
	for _, c := range []struct {
		in      string
		modes   int
		unknown []string
	}{
		{"train", 2, nil},
		{"trian", 0, []string{"trian"}},
		{"rail", 0, []string{"rail"}},
		{"train,tam", 2, []string{"tam"}},
		{"", 0, nil},
	} {
		got, unknown := ParseModeList(c.in)
		if len(got) != c.modes || len(unknown) != len(c.unknown) {
			t.Errorf("ParseModeList(%q) = %v, unknown %v; want %d modes and unknown %v",
				c.in, got, unknown, c.modes, c.unknown)
		}
	}
}
