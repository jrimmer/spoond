package grid

// sample draws the composed scene the golden tests compare against:
// a dashed panel with a title, marked as one element, containing a
// meter, a sparkline and plain text.
func sample() *Grid {
	g := New(38, 5)
	g.Box(0, 0, 38, 5, "frame", true)
	g.Title(2, 0, []Seg{{Text: " fleet ", Style: "title"}})
	g.Mark(0, 0, 38, 5, "panel")

	g.Text(2, 1, "leases", "label", -1)
	g.Meter(9, 1, "", 74, 60, 85, 27, "74%")
	g.Text(2, 2, "cpu", "label", -1)
	g.Meter(9, 2, "", 31, 60, 85, 27, "31%")
	g.Text(2, 3, "req/s", "label", -1)
	g.Segs(9, 3, []Seg{{Text: Sparkline([]float64{2, 5, 3, 8, 6, 9, 4}, 0, 10), Style: "spark"}}, -1)
	g.Text(17, 3, "<host & \"api\">", "text", -1)
	return g
}

// sampleANSIStyles is the style map the ANSI golden test uses.
func sampleANSIStyles() map[string]string {
	return map[string]string{
		"frame": "\x1b[2m",
		"title": "\x1b[1;36m",
		"label": "\x1b[0;2m",
		"ok":    "\x1b[32m",
		"warn":  "\x1b[33m",
		"bad":   "\x1b[31m",
		"spark": "\x1b[36m",
		"text":  "\x1b[37m",
	}
}
