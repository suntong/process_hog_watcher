# Process Hog Watcher (phw)

[![MIT License](http://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GoDoc](https://godoc.org/github.com/suntong/process_hog_watcher?status.svg)](http://godoc.org/github.com/suntong/process_hog_watcher)
[![Go Report Card](https://goreportcard.com/badge/github.com/suntong/process_hog_watcher)](https://goreportcard.com/report/github.com/suntong/process_hog_watcher)
[![Build Status](https://github.com/suntong/process_hog_watcher/actions/workflows/go-release-build.yml/badge.svg?branch=master)](https://github.com/suntong/process_hog_watcher/actions/workflows/go-release-build.yml)
[![PoweredBy WireFrame](https://github.com/go-easygen/wireframe/blob/master/PoweredBy-WireFrame-B.svg)](http://godoc.org/github.com/go-easygen/wireframe)

A professional-grade Go application that monitors CPU usage of processes matching a specific pattern and terminates those that exceed a defined threshold, featuring a Docker (BuildKit) Terminal User Interface (TUI) style output.

## Features

- **Docker BuildKit-Style TUI**: Flashing, self-updating interface with persistent roll-up output
- **Volatile Dashboard**: Real-time CPU usage display that updates in-place without scrolling
- **Persistent Kill Log**: Top 3 lines show recent kills; older kills scroll into terminal history
- **Scrollback Preservation**: Your existing terminal content and scrollback are preserved; kill messages accumulate above the dashboard and scroll up naturally
- **Cross-Platform**: Works on Linux, macOS, BSD, and Windows
- Monitors processes owned by the current user
- Filters processes by configurable regex pattern (e.g., `firefox|chrome`)
- Terminates processes that exceed configurable CPU threshold
- Continuous monitoring with configurable sleep intervals
- Graceful shutdown on SIGINT/SIGTERM/SIGQUIT
- Configurable via environment variables

## TUI Architecture: Docker Output Style

### Volatile Output (Live Dashboard)

The dashboard displays real-time CPU usage and updates in-place using terminal cursor positioning. The data refreshes every cycle without scrolling, giving you a stable view of all matching processes and their current CPU consumption.

**Benefits:**
- Clean, stable viewport that doesn't scroll thus doesn't ruin scrollback buffer
- Real-time visibility of all matching processes
- No terminal scrollback pollution from transient CPU data

### Persistent Roll-up Output (Kill Messages)

When a process is killed, the notification appears at the top of the screen and stays there. Each new kill pushes previous ones upward, building a visible history of the last 3 kills at the top of your terminal. Older kill messages scroll off screen but remain in your terminal's scrollback buffer - just scroll up to see the complete kill history.

Your existing terminal scrollback (from before the program started) is preserved. The kill messages appear above the dashboard and scroll up naturally, just like any other terminal output.

**Visual Layout:**
```
Line 1: Process Monitor - Real-time CPU Monitoring  ← Persistent
Line 2: Press Ctrl-C to exit                        ← Persistent
Line 3: [Most recent kill message]                  ← Persistent
Line 4: PID NAME  CPU% AVG  MAX  MIN EXCEED/TOTAL   ← Dashboard (updates live)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Line 5+: Process data...                            ← Dashboard (updates live)
```

**Benefits:**
- Last 3 kills always visible at top without scrolling
- Complete kill history accessible by scrolling up
- Clear separation between live data and event history
- Your previous terminal scrollback remains intact

## Configuration

All parameters can be customized via environment variables:

| Environment Variable | Description | Default Value |
|---------------------|-------------|---------------|
| `PROCESS_NAME_PATTERN` | Regex pattern to match process names | `firefox\|chrome` |
| `CPU_THRESHOLD` | CPU usage percentage threshold | `97.0` |
| `SLEEP_INTERVAL` | Time between monitoring cycles | `5s` |

## Usage

### Basic Usage

```bash
# Run with default settings
go run .

# Run with custom settings
PROCESS_NAME_PATTERN="firefox|chrome|safari" \
CPU_THRESHOLD=90.0 \
SLEEP_INTERVAL=10s \
go run .
```

### Building and Running

```bash
# Build the executable
go build -o phw

# Run the executable
./phw

# Run with custom settings
PROCESS_NAME_PATTERN="node" CPU_THRESHOLD=80.0 ./phw
```

## How It Works

1. **Process Discovery**: Lists all processes using `gopsutil/process.Processes()`
2. **User Filtering**: Filters to processes owned by current user
3. **Pattern Matching**: Filters by regex pattern (e.g., `firefox|chrome`)
4. **CPU Monitoring**: Samples CPU usage via `process.CPUPercent()`
5. **Threshold Check**: Identifies processes exceeding `CPU_THRESHOLD` (default: 97.0%)
6. **Process Termination**: Kills offending processes (configurable threshold via `CPU_THRESHOLD`)
7. **TUI Update**: 
   - Dashboard redraws at line 4+ (volatile, updates live)
   - Kill message scrolls terminal up (persistent, stays in history)
8. **Continuous Loop**: Repeats at configured `SLEEP_INTERVAL`


## Technical Stack

### golang.org/x/term

Provides cross-platform terminal control for the TUI:
- `IsTerminal()`: Detect TTY for appropriate output mode
- Terminal size detection and cursor positioning
- Enables Docker-style volatile + persistent output separation

### github.com/shirou/gopsutil/v4

Cross-platform system information library:
- **Cross-OS Support**: Linux, macOS, BSD, Windows (not just Unix/Linux)
- **Pure Go Implementation**: No external command dependencies (`ps`, `top`, `kill`)
- **Process.CPUPercent()**: Accurate CPU usage sampling
- **Process.Kill()**: Cross-platform process termination
- **process.Processes()**: Enumerate all running processes
- **user.Current()**: Identify current user for filtering

## Requirements

- Go 1.25 or higher
- Cross-platform support: Linux, macOS, BSD, Windows
- Permissions to send signals to owned processes

## Safety Features

- Checks process existence before attempting to terminate
- Comprehensive error handling
- Non-fatal errors don't stop the monitoring loop
- Supports graceful shutdown on SIGINT/SIGTERM/SIGQUIT

## Customization Examples

### Monitor Node.js Processes

```bash
PROCESS_NAME_PATTERN="node" \
CPU_THRESHOLD=80.0 \
./phw
```

### Monitor Python Processes with Aggressive Threshold

```bash
PROCESS_NAME_PATTERN="python" \
CPU_THRESHOLD=95.0 \
SLEEP_INTERVAL=2s \
./phw
```

### Monitor Multiple Browser Types

```bash
PROCESS_NAME_PATTERN="firefox|chrome|chromium|safari" \
CPU_THRESHOLD=90.0 \
./phw
```

## Author

Tong SUN  
![suntong from cpan.org](https://img.shields.io/badge/suntong-%40cpan.org-lightgrey.svg "suntong from cpan.org")


## License

MIT License
