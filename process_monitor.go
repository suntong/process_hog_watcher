package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

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

// ProcessMonitor handles the monitoring and control of processes
type ProcessMonitor struct {
	config  Configuration
	logger  *log.Logger
	regex   *regexp.Regexp
	ctx     context.Context
	cancel  context.CancelFunc
}

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

// ============================================================================
// Main Function
// ============================================================================

func main() {
	monitor, err := NewProcessMonitor()
	if err != nil {
		log.Fatalf("Failed to create process monitor: %v", err)
	}

	monitor.logger.Println("Process Monitor started (powered by gopsutil v4)")
	monitor.logger.Printf("Configuration: ProcessNamePattern=%s, CPUThreshold=%.1f%%, DataCollectionDuration=%v, SleepInterval=%v, CPUSamplingInterval=%v",
		monitor.config.ProcessNamePattern,
		monitor.config.CPUThreshold,
		monitor.config.DataCollectionDuration,
		monitor.config.SleepInterval,
		monitor.config.CPUSamplingInterval)

	if err := monitor.Run(); err != nil {
		monitor.logger.Fatalf("Process monitor failed: %v", err)
	}
}

// ============================================================================
// Constructor
// ============================================================================

// NewProcessMonitor creates a new process monitor with configuration from environment variables
func NewProcessMonitor() (*ProcessMonitor, error) {
	ctx, cancel := context.WithCancel(context.Background())

	config := Configuration{
		ProcessNamePattern:     getEnvWithDefault("PROCESS_NAME_PATTERN", "firefox|chrome"),
		CPUThreshold:           getEnvFloatWithDefault("CPU_THRESHOLD", 97.0),
		DataCollectionDuration: getEnvDurationWithDefault("DATA_COLLECTION_DURATION", 180*time.Second),
		SleepInterval:          getEnvDurationWithDefault("SLEEP_INTERVAL", 5*time.Second),
		CPUSamplingInterval:    getEnvDurationWithDefault("CPU_SAMPLING_INTERVAL", 3*time.Second),
	}

	regex, err := regexp.Compile(config.ProcessNamePattern)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid process name pattern: %w", err)
	}

	monitor := &ProcessMonitor{
		config: config,
		logger: log.New(os.Stdout, "[ProcessMonitor] ", log.LstdFlags|log.Lmsgprefix),
		regex:  regex,
		ctx:    ctx,
		cancel: cancel,
	}

	// Setup signal handling for graceful shutdown
	monitor.setupSignalHandler()

	return monitor, nil
}

// ============================================================================
// Main Run Loop
// ============================================================================

// Run starts the main monitoring loop
func (pm *ProcessMonitor) Run() error {
	ticker := time.NewTicker(pm.config.SleepInterval)
	defer ticker.Stop()

	pm.logger.Println("Starting monitoring loop")

	for {
		select {
		case <-pm.ctx.Done():
			pm.logger.Println("Context cancelled, stopping monitor")
			return nil
		case <-ticker.C:
			if err := pm.monitorProcesses(); err != nil {
				pm.logger.Printf("Error during monitoring cycle: %v", err)
				// Continue to next cycle even if this one fails
			}
		}
	}
}

// ============================================================================
// Core Monitoring Functions
// ============================================================================

// monitorProcesses executes one complete monitoring cycle
func (pm *ProcessMonitor) monitorProcesses() error {
	pm.logger.Println("Starting monitoring cycle")

	// Get current user
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}

	// Get all processes for current user
	processes, err := pm.getUserProcesses(currentUser.Username)
	if err != nil {
		return fmt.Errorf("failed to get user processes: %w", err)
	}

	// Filter processes by name pattern
	filteredProcesses := pm.filterProcessesByPattern(processes)
	if len(filteredProcesses) == 0 {
		pm.logger.Println("No matching processes found")
		return nil
	}

	pm.logger.Printf("Found %d matching processes to monitor", len(filteredProcesses))

	// Monitor CPU usage for each filtered process
	var wg sync.WaitGroup
	results := make(chan *CPUUsageData, len(filteredProcesses))
	errors := make(chan error, len(filteredProcesses))

	for _, proc := range filteredProcesses {
		wg.Add(1)
		go func(p ProcessInfo) {
			defer wg.Done()
			cpuData, err := pm.monitorProcessCPU(p)
			if err != nil {
				errors <- fmt.Errorf("failed to monitor process %d: %w", p.PID, err)
				return
			}
			results <- cpuData
		}(proc)
	}

	// Wait for all monitoring to complete
	wg.Wait()
	close(results)
	close(errors)

	// Collect errors
	var monitoringErrors []error
	for err := range errors {
		monitoringErrors = append(monitoringErrors, err)
	}

	// Process CPU usage data
	for cpuData := range results {
		pm.logger.Printf("Process %d (%s): CPU samples=%d, avg=%.2f%%, max=%.2f%%, min=%.2f%%, exceeded=%d/%d",
			cpuData.PID, cpuData.Name, len(cpuData.Samples),
			cpuData.AverageCPU, cpuData.MaxCPU, cpuData.MinCPU,
			cpuData.ExceedCount, len(cpuData.Samples))

		// Check if CPU usage consistently exceeds threshold
		if pm.shouldKillProcess(cpuData) {
			if err := pm.killProcess(cpuData); err != nil {
				pm.logger.Printf("Failed to kill process %d: %v", cpuData.PID, err)
			}
		}
	}

	// Log any monitoring errors (non-fatal)
	for _, err := range monitoringErrors {
		pm.logger.Printf("Monitoring error: %v", err)
	}

	pm.logger.Println("Monitoring cycle completed")
	return nil
}

// getUserProcesses retrieves all processes owned by the specified user using gopsutil
func (pm *ProcessMonitor) getUserProcesses(username string) ([]ProcessInfo, error) {
	pm.logger.Printf("Getting processes for user: %s", username)

	allProcesses, err := process.Processes()
	if err != nil {
		return nil, fmt.Errorf("failed to get processes: %w", err)
	}

	var processes []ProcessInfo
	for _, p := range allProcesses {
		name, err := p.Name()
		if err != nil {
			pm.logger.Printf("Failed to get name for PID %d: %v", p.Pid, err)
			continue
		}

		username, err := p.Username()
		if err != nil {
			pm.logger.Printf("Failed to get username for PID %d: %v", p.Pid, err)
			continue
		}

		// Filter processes by current user
		if username == currentUserUsername() {
			processes = append(processes, ProcessInfo{
				PID:     p.Pid,
				Name:    name,
				User:    username,
				Process: p,
			})
		}
	}

	pm.logger.Printf("Retrieved %d processes for user %s", len(processes), username)
	return processes, nil
}

// filterProcessesByPattern filters processes by name regex pattern
func (pm *ProcessMonitor) filterProcessesByPattern(processes []ProcessInfo) []ProcessInfo {
	pm.logger.Printf("Filtering %d processes by pattern: %s", len(processes), pm.config.ProcessNamePattern)

	var filtered []ProcessInfo
	for _, proc := range processes {
		if pm.regex.MatchString(proc.Name) {
			filtered = append(filtered, proc)
		}
	}

	pm.logger.Printf("Filtered down to %d matching processes", len(filtered))
	return filtered
}

// monitorProcessCPU monitors CPU usage for a single process over the data collection duration
func (pm *ProcessMonitor) monitorProcessCPU(proc ProcessInfo) (*CPUUsageData, error) {
	pm.logger.Printf("Starting CPU monitoring for process %d (%s)", proc.PID, proc.Name)

	cpuData := &CPUUsageData{
		PID:     proc.PID,
		Name:    proc.Name,
		Samples: make([]float64, 0),
	}

	// Calculate number of samples to collect
	numSamples := int(pm.config.DataCollectionDuration / pm.config.CPUSamplingInterval)
	if numSamples < 1 {
		numSamples = 1
	}

	pm.logger.Printf("Collecting %d CPU samples over %v (interval: %v)",
		numSamples, pm.config.DataCollectionDuration, pm.config.CPUSamplingInterval)

	// Collect CPU samples
	for i := 0; i < numSamples; i++ {
		// Check if process still exists
		if !pm.processExists(proc.PID) {
			return cpuData, fmt.Errorf("process %d no longer exists", proc.PID)
		}

		cpuUsage, err := pm.getProcessCPUUsage(proc.Process)
		if err != nil {
			pm.logger.Printf("Failed to get CPU usage for process %d: %v", proc.PID, err)
			continue
		}

		cpuData.Samples = append(cpuData.Samples, cpuUsage)
		pm.logger.Printf("Process %d (%s): CPU usage = %.2f%%", proc.PID, proc.Name, cpuUsage)

		// Sleep between samples (but not after the last sample)
		if i < numSamples-1 {
			time.Sleep(pm.config.CPUSamplingInterval)
		}
	}

	// Calculate statistics
	pm.calculateCPUStatistics(cpuData)

	return cpuData, nil
}

// getProcessCPUUsage gets current CPU usage for a process using gopsutil
func (pm *ProcessMonitor) getProcessCPUUsage(p *process.Process) (float64, error) {
	// CPUPercent needs to be called twice to calculate the usage
	// First call initializes internal state
	_, err := p.CPUPercent()
	if err != nil {
		return 0, fmt.Errorf("failed to initialize CPU percent: %w", err)
	}

	// Wait for the configured sampling interval
	time.Sleep(pm.config.CPUSamplingInterval)

	// Second call returns the actual CPU usage
	cpuPercent, err := p.CPUPercent()
	if err != nil {
		return 0, fmt.Errorf("failed to get CPU percent: %w", err)
	}

	return cpuPercent, nil
}

// shouldKillProcess determines if a process should be killed based on CPU usage data
func (pm *ProcessMonitor) shouldKillProcess(cpuData *CPUUsageData) bool {
	if len(cpuData.Samples) == 0 {
		return false
	}

	// Check if ALL samples exceed the threshold
	allExceeded := true
	for _, sample := range cpuData.Samples {
		if sample < pm.config.CPUThreshold {
			allExceeded = false
			break
		}
	}

	if allExceeded {
		pm.logger.Printf("Process %d (%s) CPU usage consistently exceeds threshold (%.1f%%)",
			cpuData.PID, cpuData.Name, pm.config.CPUThreshold)
		return true
	}

	return false
}

// killProcess terminates a process with the given PID using gopsutil
func (pm *ProcessMonitor) killProcess(cpuData *CPUUsageData) error {
	pm.logger.Printf("Killing process %d (%s) due to excessive CPU usage", cpuData.PID, cpuData.Name)

	// Check if process still exists
	exists, err := process.PidExists(cpuData.PID)
	if err != nil {
		return fmt.Errorf("failed to check process existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("process %d no longer exists", cpuData.PID)
	}

	p, err := process.NewProcess(cpuData.PID)
	if err != nil {
		return fmt.Errorf("failed to create process handle: %w", err)
	}

	// Send SIGTERM first (graceful shutdown)
	if err := p.Terminate(); err != nil {
		pm.logger.Printf("Failed to send SIGTERM to process %d: %v", cpuData.PID, err)
		// Try SIGKILL as fallback
		if err := p.Kill(); err != nil {
			return fmt.Errorf("failed to kill process %d with SIGKILL: %w", cpuData.PID, err)
		}
		pm.logger.Printf("Process %d killed with SIGKILL", cpuData.PID)
	} else {
		pm.logger.Printf("Process %d terminated with SIGTERM", cpuData.PID)
		// Wait a bit to see if process exits gracefully
		time.Sleep(2 * time.Second)

		exists, _ = process.PidExists(cpuData.PID)
		if exists {
			pm.logger.Printf("Process %d still running, sending SIGKILL", cpuData.PID)
			if err := p.Kill(); err != nil {
				return fmt.Errorf("failed to kill process %d with SIGKILL: %w", cpuData.PID, err)
			}
		}
	}

	pm.logger.Printf("Successfully killed process %d (%s)", cpuData.PID, cpuData.Name)
	return nil
}

// processExists checks if a process with the given PID exists using gopsutil
func (pm *ProcessMonitor) processExists(pid int32) bool {
	exists, err := process.PidExists(pid)
	if err != nil {
		return false
	}

	return exists
}

// ============================================================================
// Helper Functions
// ============================================================================

// calculateCPUStatistics calculates statistics from CPU usage samples
func (pm *ProcessMonitor) calculateCPUStatistics(cpuData *CPUUsageData) {
	if len(cpuData.Samples) == 0 {
		return
	}

	sum := 0.0
	maxCPU := cpuData.Samples[0]
	minCPU := cpuData.Samples[0]
	exceedCount := 0

	for _, sample := range cpuData.Samples {
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

	cpuData.AverageCPU = sum / float64(len(cpuData.Samples))
	cpuData.MaxCPU = maxCPU
	cpuData.MinCPU = minCPU
	cpuData.ExceedCount = exceedCount
}

// setupSignalHandler sets up signal handling for graceful shutdown
func (pm *ProcessMonitor) setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		pm.logger.Printf("Received signal: %v, shutting down...", sig)
		pm.cancel()
	}()
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
		if float, err := strconv.ParseFloat(value, 64); err == nil {
			return float
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