package api

import "testing"

// ---------- Folder naming helpers ----------

func TestEpisodeFolderName(t *testing.T) {
	cases := []struct {
		i, episodes int
		want        string
	}{
		{1, 12, "Ep 01"}, // normal series: two digits
		{12, 12, "Ep 12"},
		{99, 99, "Ep 99"},
		{1, 100, "Ep 001"}, // three digits once the count reaches 100
		{245, 245, "Ep 245"},
		{1, 999, "Ep 001"},
	}
	for _, c := range cases {
		if got := episodeFolderName(c.i, c.episodes); got != c.want {
			t.Errorf("episodeFolderName(%d, %d) = %q, want %q", c.i, c.episodes, got, c.want)
		}
	}
}

func TestValidateSeriesName(t *testing.T) {
	// Valid names pass — unicode, spaces, brackets and dots are all legal
	// Windows folder characters used in anime release names.
	for _, ok := range []string{
		"Ookami-san to Shichinin no Nakama-tachi",
		"Series [Season 2]",
		"86 -エイティシックス-",
		"2160p",
	} {
		if err := validateSeriesName(ok); err != nil {
			t.Errorf("validateSeriesName(%q) rejected a valid name: %v", ok, err)
		}
	}
	// Unsafe / nonsense names are refused. Note "Re:Zero..." is a real
	// series title whose colon is illegal in Windows folder names — the
	// validator catches exactly this class of problem.
	for _, bad := range []string{
		"",
		"   ",
		"Re:Zero kara Hajimeru Isekai Seikatsu", // colon is reserved on Windows
		"a/b",
		`a\b`,
		"..",
		"a..b",
		"a:b",
		"a*b",
		"a?b",
		"a\"b",
		"a<b",
		"a>b",
		"a|b",
		".hidden",
		"trailing ",
		"trailing.",
	} {
		if err := validateSeriesName(bad); err == nil {
			t.Errorf("validateSeriesName(%q) accepted an invalid name", bad)
		}
	}
	// Over-long names are refused.
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	if err := validateSeriesName(string(long)); err == nil {
		t.Error("over-long name accepted")
	}
}
