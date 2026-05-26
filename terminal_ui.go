package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// ============================================================================
// Constants for ANSI escape sequences and terminal defaults
// ============================================================================

const (
	defaultCPUColorRed    = "\033[31m"
	defaultCPUColorYellow = "\033[33m"
	defaultCPUColorGreen  = "\033[32m"
	defaultColorReset     = "\033[0m"
	defaultClearLine      = "\033[2K"
	defaultCursorHome     = "\x1b[%d;1H"
)

// ============================================================================
// Terminal Dashboard – Two‑Tier Output with Full Synchronisation
// ============================================================================

// TerminalDashboard manages all terminal output: the live dashboard (volatile)
// and persistent messages (startup, kills, errors) that enter the scroll‑back buffer.
type TerminalDashboard struct {
	mu        sync.Mutex // protects all fields and terminal writes
	startLine int        // line where dashboard begins (1‑based) – set in Init()
	height    int        // number of rows dashboard occupies
	width     int        // terminal width in columns
	active    bool       // true after Init() called
}

// NewTerminalDashboard creates a new dashboard instance (not yet active).
func NewTerminalDashboard() *TerminalDashboard {
	return &TerminalDashboard{
		startLine: 0,
		active:    false,
	}
}

// getTerminalSize returns the current terminal height and width in characters.
// If detection fails, returns safe defaults.
func getTerminalSize() (height, width int, err error) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return 24, 80, nil
	}
	width, height, err = term.GetSize(fd)
	if err != nil {
		return 24, 80, nil
	}
	return height, width, nil
}

// SetStartLine sets the line where the dashboard should begin (1‑based).
// Must be called before Init().
func (d *TerminalDashboard) SetStartLine(line int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startLine = line
	if d.startLine < 1 {
		d.startLine = 1
	}
}

// Init reads terminal size, sets up the dashboard area, and marks it active.
// It clears from startLine to the bottom of the terminal, preserving lines above.
func (d *TerminalDashboard) Init() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active {
		return
	}
	if d.startLine == 0 {
		// Safety: default to line 1 if not set
		d.startLine = 1
	}
	h, w, _ := getTerminalSize()
	d.height = h - d.startLine + 1
	if d.height < 1 {
		d.height = 1
	}
	d.width = w
	d.active = true

	// Move cursor to first dashboard line and clear the entire dashboard area
	fmt.Printf(defaultCursorHome, d.startLine)
	for i := 0; i < d.height; i++ {
		fmt.Print(strings.Repeat(" ", d.width) + defaultClearLine)
		if i < d.height-1 {
			fmt.Print("\n")
		}
	}
	fmt.Printf(defaultCursorHome, d.startLine)
}

// Resize is called on SIGWINCH; it re‑initialises the dashboard with new dimensions.
// The start line remains unchanged.
func (d *TerminalDashboard) Resize() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return
	}
	h, w, _ := getTerminalSize()
	newHeight := h - d.startLine + 1
	if newHeight < 1 {
		newHeight = 1
	}
	if newHeight == d.height && w == d.width {
		return
	}
	d.height = newHeight
	d.width = w

	// Redraw the dashboard area with the new size
	fmt.Printf(defaultCursorHome, d.startLine)
	for i := 0; i < d.height; i++ {
		fmt.Print(strings.Repeat(" ", d.width) + defaultClearLine)
		if i < d.height-1 {
			fmt.Print("\n")
		}
	}
	fmt.Printf(defaultCursorHome, d.startLine)
}

// Draw redraws the entire volatile dashboard.
// rows must contain exactly d.height strings (shorter ones are padded with empty lines).
func (d *TerminalDashboard) Draw(rows []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return
	}

	// Pad or truncate rows to exactly d.height
	for len(rows) < d.height {
		rows = append(rows, "")
	}
	if len(rows) > d.height {
		rows = rows[:d.height]
	}

	fmt.Printf(defaultCursorHome, d.startLine)
	for i, row := range rows {
		// Clear the entire line before drawing
		fmt.Print(defaultClearLine)
		// Truncate or pad to terminal width
		if len(row) > d.width {
			row = row[:d.width-1] + "…"
		} else {
			row = row + strings.Repeat(" ", d.width-len(row))
		}
		fmt.Print(row)
		if i < d.height-1 {
			fmt.Print("\n")
		}
	}
}

// LogPersistent prints a message to the scroll‑back buffer using a terminal scroll.
// It prints the message at the dashboard start line, then scrolls the terminal
// up by one line (by printing a newline at the bottom), pushing the message
// above the dashboard, where it becomes persistent in the scrollback.
func (d *TerminalDashboard) LogPersistent(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		log.Printf("%s", msg)
		return
	}

	// 1. Move to dashboard header line (e.g., line 4) & clear the line
	fmt.Printf(defaultCursorHome, d.startLine)
	fmt.Print(defaultClearLine)

	// 2. Print the message without newline
	log.Print(msg)

	// 3. Move cursor to the bottom line of the terminal
	h, _, _ := getTerminalSize()
	fmt.Printf(defaultCursorHome, h)

	// 4. Print a newline -> terminal scrolls up by one line
	fmt.Println()

	// 5. Move cursor back to dashboard header line (for next draw)
	fmt.Printf(defaultCursorHome, d.startLine)
	// Note: The dashboard content has shifted up one line.
	// The next call to Draw() will clear from startLine down and redraw,
	// completely refreshing the dashboard at the correct position.
}

// Shutdown moves the cursor to the bottom line and prints the goodbye message.
// It does NOT clear the dashboard.
func (d *TerminalDashboard) Shutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, _, _ := getTerminalSize()
	// Move cursor to the last line of the terminal
	fmt.Printf(defaultCursorHome, h)
	log.Println("Interrupted. Goodbye.                                           ")
}

// colorizeCPU returns an ANSI‑colored string for the given CPU percentage.
// threshold is the value above which red is used (normally the kill threshold).
func colorizeCPU(value float64, threshold float64) string {
	var color string
	switch {
	case value >= threshold:
		color = defaultCPUColorRed
	case value > 75:
		color = defaultCPUColorYellow
	default:
		color = defaultCPUColorGreen
	}
	return fmt.Sprintf("%s%.1f%%%s", color, value, defaultColorReset)
}
