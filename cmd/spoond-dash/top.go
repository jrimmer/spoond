// spoond top: the same character grid as the dashboard, drawn with ANSI
// styles at the terminal's width (COLUMNS, else 104) and redrawn every
// 2 s until interrupted. It reads the same sources as spoond dash: the
// collector is shared, only the rendering target differs — the terminal
// instead of a browser page. No colour when stdout is not a terminal.
package spoonddash

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// topStyles maps the grid's style names to SGR sequences: the palette in
// grid.css, translated for a terminal. Unknown styles (the empty style
// included) print bare.
func topStyles() map[string]string {
	if !isTerminal(os.Stdout) {
		return map[string]string{} // no escapes when piped or redirected
	}
	return map[string]string{
		"head":  "\x1b[1;97m",
		"frame": "\x1b[2;37m",
		"title": "\x1b[1;97m",
		"dim":   "\x1b[2;90m",
		"text":  "\x1b[0;97m",
		"ok":    "\x1b[0;32m",
		"warn":  "\x1b[0;33m",
		"bad":   "\x1b[0;31m",
		"state": "\x1b[0;94m",
		"spark": "\x1b[0;92m",
		"link":  "\x1b[0;36m",
	}
}

// isTerminal reports whether f is a character device.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Top runs spoond top: one collector loop of its own, a frame every
// DASH_INTERVAL (like the page), ANSI-drawn at width until interrupted.
// The exit code follows Main's conventions (0 on interrupt).
func Top(args []string) int {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond top: %v\n", err)
		return 2
	}
	width := clamp(terminalWidth(), minW, maxW)
	col := newCollector(cfg)
	styles := topStyles()
	hist := map[string][]float64{}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The clear goes to a terminal only: piped or redirected output is
	// the plain frame, one after another.
	clear := ""
	if isTerminal(os.Stdout) {
		clear = "\x1b[H\x1b[2J"
	}

	draw := func() {
		s := col.collect(ctx)
		appendHist(hist, s, cfg.History)
		g, err := drawFrame(s, hist, width, cfg.Host, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "spoond top:", err)
			return
		}
		fmt.Print(clear + g.ANSI(styles) + "\n")
	}
	draw()
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return 0
		case <-t.C:
			draw()
		}
	}
}

// terminalWidth is the frame width: COLUMNS when set (the shell usually
// exports it; Go has no termios dependency to spare), else the default.
// It is read once at startup; a resized terminal is picked up on the
// next restart.
func terminalWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultWidth
}
