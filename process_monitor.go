package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// ============================================================================
// Constants & Defaults (no hard‑coded values – all can be overridden by env vars)
// ============================================================================

// Default configuration values (may be overridden by env vars)
var (
	defaultProcessNamePattern     = "firefox|chrome"
	defaultCPUThreshold           = 97.0
	defaultDataCollectionDuration = 180 * time.Second
	defaultSleepInterval          = 5 * time.Second
	defaultCPUSamplingInterval    = 3 * time.Second
)

// ============================================================================
// Configuration Struct
// ============================================================================

// Configuration contains all customizable parameters for the process monitor
type Configuration struct {
	// ProcessNamePattern is the regex pattern to match process names
	ProcessNamePattern string

	// CPUThreshold is the CPU usage percentage threshold to trigger process termination
	CPUThreshold float64

	// DataCollectionDuration is how long to collect CPU usage data before making a decision
	DataCollectionDuration time.Duration

	// SleepInterval is how long to wait between monitoring cycles
	SleepInterval time.Duration

	// CPUSamplingInterval is how often to sample CPU usage during data collection
	CPUSamplingInterval time.Duration
}

// ============================================================================
// Process Monitor Core
// ============================================================================

// ProcessInfo represents information about a process
type ProcessInfo struct {
	PID     int32
	Name    string
	User    string
	Process *process.Process
}

// CPUUsageData stores CPU usage samples for a process
type CPUUsageData struct {
	PID         int32
	Name        string
	Samples     []float64
	AverageCPU  float64
	MaxCPU      float64
	MinCPU      float64
	ExceedCount int
}

// ProcessMonitor handles the monitoring and control of processes
type ProcessMonitor struct {
	config    Configuration
	regex     *regexp.Regexp
	ctx       context.Context
	cancel    context.CancelFunc
	dashboard *TerminalDashboard

	mu         sync.RWMutex // protects cpuDataMap
	cpuDataMap map[int32]*CPUUsageData
}

// ============================================================================
// Main Function
// ============================================================================

func main() {
	monitor, err := NewProcessMonitor()
	if err != nil {
		log.Fatalf("Failed to create process monitor: %v", err)
	}
	if err := monitor.Run(); err != nil {
		log.Fatalf("Process monitor failed: %v", err)
	}
}

// ============================================================================
// Constructor
// ============================================================================

const preservedLines = 3 // number of lines kept above the dashboard (always 3)

// NewProcessMonitor creates a new process monitor with configuration from environment variables
func NewProcessMonitor() (*ProcessMonitor, error) {
	ctx, cancel := context.WithCancel(context.Background())

	config := Configuration{
		ProcessNamePattern:     getEnvWithDefault("PROCESS_NAME_PATTERN", defaultProcessNamePattern),
		CPUThreshold:           getEnvFloatWithDefault("CPU_THRESHOLD", defaultCPUThreshold),
		DataCollectionDuration: getEnvDurationWithDefault("DATA_COLLECTION_DURATION", defaultDataCollectionDuration),
		SleepInterval:          getEnvDurationWithDefault("SLEEP_INTERVAL", defaultSleepInterval),
		CPUSamplingInterval:    getEnvDurationWithDefault("CPU_SAMPLING_INTERVAL", defaultCPUSamplingInterval),
	}

	regex, err := regexp.Compile(config.ProcessNamePattern)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid process name pattern: %w", err)
	}

	// -------------------------------------------------------------------------
	// Startup messages and terminal scrolling (to place them at the top)
	// -------------------------------------------------------------------------
	// Get terminal height
	termHeight, termWidth, _ := getTerminalSize()

	// Define the 3 preserved lines (order: first becomes top‑most after scroll)
	startupLines := []string{
		fmt.Sprintf("Process Monitor started | Pattern=%s | CPU Threshold=%.1f%% | DataDuration=%v | Sleep=%v | SampleInterval=%v",
			config.ProcessNamePattern, config.CPUThreshold,
			config.DataCollectionDuration, config.SleepInterval, config.CPUSamplingInterval),
		fmt.Sprintf("Terminal: %dx%d | Dashboard starts: line %d",
			termHeight, termWidth, preservedLines+1),
		"Process Monitor - Real-time CPU Monitoring",
		"Press Ctrl-C to exit",
	}

	// Print them at the current cursor position (bottom)
	for _, line := range startupLines {
		log.Println(line)
	}

	// Scroll up by (termHeight - preservedLines) blank lines
	blankLines := termHeight - preservedLines - 1
	for i := 0; i < blankLines; i++ {
		fmt.Println()
	}

	// Dashboard starts at line preservedLines+1 = 4
	startLine := preservedLines + 1
	fmt.Printf("\x1b[%d;1H", startLine)

	// -------------------------------------------------------------------------
	// Create the monitor and dashboard
	// -------------------------------------------------------------------------
	pm := &ProcessMonitor{
		config:     config,
		regex:      regex,
		ctx:        ctx,
		cancel:     cancel,
		dashboard:  NewTerminalDashboard(),
		cpuDataMap: make(map[int32]*CPUUsageData),
	}
	pm.dashboard.SetStartLine(startLine)
	pm.dashboard.Init()

	// Setup signal handling for graceful shutdown
	pm.setupSignalHandler()

	return pm, nil
}

// setupSignalHandler sets up signal handling for graceful shutdown
func (pm *ProcessMonitor) setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	// Common signals: interrupt, terminate, quit
	// (Windows Ctrl+Break is not handled on Linux; omitted for cross‑platform build)
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT}
	signal.Notify(sigChan, signals...)

	go func() {
		select {
		case <-sigChan:
			pm.dashboard.Shutdown()
			pm.cancel()
		case <-pm.ctx.Done():
			return
		}
	}()
}

// ============================================================================
// Main Run Loop
// ============================================================================

// Run starts the main monitoring loop
func (pm *ProcessMonitor) Run() error {
	// Start background CPU sampler
	go pm.cpuSampler()

	// Main dashboard refresh and kill decision loop
	ticker := time.NewTicker(pm.config.SleepInterval)
	defer ticker.Stop()

	// Show initial message
	pm.dashboard.Draw([]string{"Collecting initial CPU data..."})

	for {
		select {
		case <-pm.ctx.Done():
			return nil
		case <-ticker.C:
			pm.refreshDashboardAndKill()
		}
	}
}

// ============================================================================
// Background Sampler
// ============================================================================

// cpuSampler continuously collects CPU samples for all matching processes.
func (pm *ProcessMonitor) cpuSampler() {
	ticker := time.NewTicker(pm.config.CPUSamplingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-pm.ctx.Done():
			return
		case <-ticker.C:
			pm.collectCPUSamples()
		}
	}
}

// collectCPUSamples gets one CPU sample for each matching process and updates the sliding window.
func (pm *ProcessMonitor) collectCPUSamples() {
	// Get current user and matching processes (without lock because we are about to acquire write lock)
	currentUser, err := user.Current()
	if err != nil {
		pm.dashboard.LogPersistent(fmt.Sprintf("Failed to get current user: %v", err))
		return
	}
	processes, err := pm.getUserProcesses(currentUser.Username)
	if err != nil {
		pm.dashboard.LogPersistent(fmt.Sprintf("Failed to get user processes: %v", err))
		return
	}
	filtered := pm.filterProcessesByPattern(processes)

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Test: log a fake PID to verify persistent logging
	//pm.testPersistentLogging()

	// Ensure all filtered processes exist in the map
	for _, p := range filtered {
		if _, exists := pm.cpuDataMap[p.PID]; !exists {
			pm.cpuDataMap[p.PID] = &CPUUsageData{
				PID:     p.PID,
				Name:    p.Name,
				Samples: make([]float64, 0),
			}
		}
	}

	// Collect one sample per process
	for _, p := range filtered {
		cpuPercent, err := p.Process.CPUPercent()
		if err != nil {
			// Process may have terminated; skip this sample
			continue
		}
		data := pm.cpuDataMap[p.PID]
		// Append sample and maintain sliding window
		data.Samples = append(data.Samples, cpuPercent)
		windowSize := int(pm.config.DataCollectionDuration / pm.config.CPUSamplingInterval)
		if len(data.Samples) > windowSize {
			data.Samples = data.Samples[len(data.Samples)-windowSize:]
		}
		// Update statistics
		pm.calculateCPUStatistics(data)
	}

	// Remove processes that no longer exist
	for pid := range pm.cpuDataMap {
		exists, err := process.PidExists(pid)
		if err != nil || !exists {
			delete(pm.cpuDataMap, pid)
		}
	}
}

// calculateCPUStatistics calculates statistics from CPU usage samples (caller must hold lock)
func (pm *ProcessMonitor) calculateCPUStatistics(data *CPUUsageData) {
	if len(data.Samples) == 0 {
		return
	}
	sum := 0.0
	maxCPU := data.Samples[0]
	minCPU := data.Samples[0]
	exceedCount := 0
	for _, sample := range data.Samples {
		sum += sample
		if sample > maxCPU {
			maxCPU = sample
		}
		if sample < minCPU {
			minCPU = sample
		}
		if sample >= pm.config.CPUThreshold {
			exceedCount++
		}
	}
	data.AverageCPU = sum / float64(len(data.Samples))
	data.MaxCPU = maxCPU
	data.MinCPU = minCPU
	data.ExceedCount = exceedCount
}

// ============================================================================
// Dashboard Refresh & Kill Decisions
// ============================================================================

// refreshDashboardAndKill reads the current CPU data, sorts by current CPU%, draws dashboard, and kills exceeding processes.
func (pm *ProcessMonitor) refreshDashboardAndKill() {
	// Take a snapshot of the data under read lock
	type processSnapshot struct {
		data       *CPUUsageData
		currentCPU float64
	}
	var snapshots []processSnapshot

	pm.mu.RLock()
	for _, data := range pm.cpuDataMap {
		if len(data.Samples) == 0 {
			continue
		}
		currentCPU := data.Samples[len(data.Samples)-1]
		snapshots = append(snapshots, processSnapshot{
			data:       data,
			currentCPU: currentCPU,
		})
	}
	pm.mu.RUnlock()

	// Sort snapshots by currentCPU descending
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].currentCPU > snapshots[j].currentCPU
	})

	// Kill any process that should be killed (needs write lock)
	for _, snap := range snapshots {
		if pm.shouldKillProcess(snap.data) {
			go pm.killProcessAsync(snap.data)
		}
	}

	// Build dashboard rows
	if len(snapshots) == 0 {
		pm.dashboard.Draw([]string{"No matching processes found"})
		return
	}

	header := fmt.Sprintf("\033[36m%-8s %-20s %-8s %-8s %-8s %-8s %s\033[0m",
		"PID", "NAME", "CPU%", "AVG", "MAX", "MIN", "EXCEED/TOTAL")
	separator := strings.Repeat("-", pm.dashboard.width)
	rows := []string{header, separator}

	for _, snap := range snapshots {
		data := snap.data
		coloredCPU := colorizeCPU(snap.currentCPU, pm.config.CPUThreshold)
		line := fmt.Sprintf("%-8d %-20s %8s %8.1f %8.1f %8.1f %3d/%d",
			data.PID, truncate(data.Name, 20), coloredCPU,
			data.AverageCPU, data.MaxCPU, data.MinCPU,
			data.ExceedCount, len(data.Samples))
		rows = append(rows, line)
	}

	pm.dashboard.Draw(rows)
}

// shouldKillProcess determines if a process should be killed based on CPU usage data.
// It does not need a lock because it only reads the data slice (which is immutable after snapshot).
func (pm *ProcessMonitor) shouldKillProcess(data *CPUUsageData) bool {
	if len(data.Samples) == 0 {
		return false
	}
	for _, sample := range data.Samples {
		if sample < pm.config.CPUThreshold {
			return false
		}
	}
	return true
}

// killProcessAsync terminates a process and logs the event persistently.
func (pm *ProcessMonitor) killProcessAsync(data *CPUUsageData) {
	killMsg := fmt.Sprintf("[%s] Killing process %d (%s) – CPU consistently above %.1f%%",
		time.Now().Format("15:04:05"), data.PID, data.Name, pm.config.CPUThreshold)
	pm.dashboard.LogPersistent(killMsg)

	p, err := process.NewProcess(data.PID)
	if err != nil {
		pm.dashboard.LogPersistent(fmt.Sprintf("Failed to create process handle for %d: %v", data.PID, err))
		return
	}
	// Try graceful termination first
	if err := p.Terminate(); err != nil {
		// Fallback to SIGKILL
		if err := p.Kill(); err != nil {
			pm.dashboard.LogPersistent(fmt.Sprintf("Failed to kill process %d: %v", data.PID, err))
			return
		}
	}
	// Remove from map under write lock
	pm.mu.Lock()
	delete(pm.cpuDataMap, data.PID)
	pm.mu.Unlock()
}

// ============================================================================
// Helper Functions
// ============================================================================

// getUserProcesses retrieves all processes owned by the specified user using gopsutil
func (pm *ProcessMonitor) getUserProcesses(username string) ([]ProcessInfo, error) {
	allProcesses, err := process.Processes()
	if err != nil {
		return nil, fmt.Errorf("failed to get processes: %w", err)
	}

	var processes []ProcessInfo
	for _, p := range allProcesses {
		name, err := p.Name()
		if err != nil {
			continue
		}
		user, err := p.Username()
		if err != nil {
			continue
		}
		if user == currentUserUsername() {
			processes = append(processes, ProcessInfo{
				PID:     p.Pid,
				Name:    name,
				User:    user,
				Process: p,
			})
		}
	}
	return processes, nil
}

// filterProcessesByPattern filters processes by name regex pattern
func (pm *ProcessMonitor) filterProcessesByPattern(processes []ProcessInfo) []ProcessInfo {
	var filtered []ProcessInfo
	for _, proc := range processes {
		if pm.regex.MatchString(proc.Name) {
			filtered = append(filtered, proc)
		}
	}
	return filtered
}

// truncate shortens a string to at most maxLen characters, adding an ellipsis if needed.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// currentUserUsername gets the current username (cached)
var cachedUsername string

func currentUserUsername() string {
	if cachedUsername != "" {
		return cachedUsername
	}
	currentUser, err := user.Current()
	if err != nil {
		return ""
	}
	cachedUsername = currentUser.Username
	return cachedUsername
}

// getEnvWithDefault gets an environment variable value or returns a default
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvFloatWithDefault gets an environment variable value as float64 or returns a default
func getEnvFloatWithDefault(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

// getEnvDurationWithDefault gets an environment variable value as time.Duration or returns a default
func getEnvDurationWithDefault(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

// testPersistentLogging is a temporary test function that logs a fake process ID
// that increases with every call, just to verify that LogPersistent writes to the
// scroll‑back buffer correctly.
func (pm *ProcessMonitor) testPersistentLogging() {
	//pm.mu.Lock()
	// Use a package‑level counter (or a field) to generate sequential IDs.
	// A simple package‑level variable is fine for this temporary test.
	// We'll define it outside.
	//pm.mu.Unlock()

	fakePID := nextTestPID() // see below
	msg := fmt.Sprintf("TEST PID %04d – persistent log check", fakePID)
	pm.dashboard.LogPersistent(msg)
}

var (
	testPIDMu    sync.Mutex
	testPIDCount = 1000
)

func nextTestPID() int {
	testPIDMu.Lock()
	defer testPIDMu.Unlock()
	testPIDCount++
	return testPIDCount
}
