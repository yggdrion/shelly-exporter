package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ShellyStatus represents the status response from a Shelly device
type ShellyStatus struct {
	Meters []struct {
		Power     float64   `json:"power"`
		IsValid   bool      `json:"is_valid"`
		Timestamp int64     `json:"timestamp"`
		Counters  []float64 `json:"counters"`
	} `json:"meters"`
	Relays []struct {
		IsOn           bool   `json:"ison"`
		HasTimer       bool   `json:"has_timer"`
		TimerStarted   int64  `json:"timer_started"`
		TimerDuration  int    `json:"timer_duration"`
		TimerRemaining int    `json:"timer_remaining"`
		Overpower      bool   `json:"overpower"`
		Source         string `json:"source"`
	} `json:"relays"`
}

// ShellyInfo represents device info from a Shelly device
type ShellyInfo struct {
	Type        string `json:"type"`
	Mac         string `json:"mac"`
	AuthEnabled bool   `json:"auth"`
	FwVersion   string `json:"fw"`
	NumOutputs  int    `json:"num_outputs"`
	NumMeters   int    `json:"num_meters"`
}

// ShellySettings represents device settings from a Shelly device
type ShellySettings struct {
	Device struct {
		Type     string `json:"type"`
		Mac      string `json:"mac"`
		Hostname string `json:"hostname"`
		Name     string `json:"name"`
	} `json:"device"`
	Name     string `json:"name"`     // Some devices put the name at root level
	Hostname string `json:"hostname"` // Fallback hostname
}

// ShellyDevice represents a discovered Shelly device
type ShellyDevice struct {
	IP         string
	DeviceID   string
	DeviceName string
	DeviceType string
	LastSeen   time.Time
}

// ShellyExporter implements prometheus.Collector
type ShellyExporter struct {
	powerGauge        *prometheus.GaugeVec
	mutex             sync.RWMutex
	devicesMutex      sync.RWMutex
	knownDevices      map[string]*ShellyDevice
	networkRange      string
	discoveryInterval time.Duration
	metricsInterval   time.Duration
}

// NewShellyExporter creates a new Shelly exporter
func NewShellyExporter(networkRange string, discoveryInterval, metricsInterval time.Duration) *ShellyExporter {
	return &ShellyExporter{
		powerGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "shelly_power_watts",
				Help: "Current power consumption in watts from Shelly devices",
			},
			[]string{"device_id", "device_name", "device_type", "ip_address"},
		),
		knownDevices:      make(map[string]*ShellyDevice),
		networkRange:      networkRange,
		discoveryInterval: discoveryInterval,
		metricsInterval:   metricsInterval,
	}
}

// Describe implements prometheus.Collector
func (e *ShellyExporter) Describe(ch chan<- *prometheus.Desc) {
	e.powerGauge.Describe(ch)
}

// Collect implements prometheus.Collector
func (e *ShellyExporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	e.powerGauge.Collect(ch)
}

// discoverDevices scans the network for Shelly devices and updates the known devices list
func (e *ShellyExporter) discoverDevices(ctx context.Context) {
	// log.Printf("Starting device discovery scan...")
	start := time.Now()

	var wg sync.WaitGroup
	foundDevices := 0
	var foundMutex sync.Mutex
	tempDevices := make(map[string]*ShellyDevice)

	// Get local network range
	ips := e.getIPRange()

	// Scan each IP address
	for _, ip := range ips {
		wg.Add(1)
		go func(ipAddr string) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			default:
			}

			if device := e.discoverShellyDevice(ipAddr); device != nil {
				foundMutex.Lock()
				foundDevices++
				tempDevices[device.IP] = device
				foundMutex.Unlock()
			}
		}(ip)
	}

	wg.Wait()

	// Update known devices list
	e.devicesMutex.Lock()
	e.knownDevices = tempDevices
	e.devicesMutex.Unlock()

	duration := time.Since(start).Seconds()

	log.Printf("Device discovery completed in %.2f seconds, found %d Shelly devices", duration, foundDevices)
}

// discoverShellyDevice checks if the given IP is a Shelly device and returns device info
func (e *ShellyExporter) discoverShellyDevice(ip string) *ShellyDevice {
	client := &http.Client{Timeout: 2 * time.Second}

	// Check if it's a Shelly device
	resp, err := client.Get(fmt.Sprintf("http://%s/shelly", ip))
	if err != nil {
		return nil
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("Error closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var info ShellyInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil
	}

	if info.Type == "" {
		return nil
	}

	// Generate device ID from MAC address
	deviceID := fmt.Sprintf("shelly%s-%s", strings.ToLower(info.Type), strings.ToLower(info.Mac[len(info.Mac)-6:]))

	// Get device settings for device name
	var deviceName string
	settingsResp, err := client.Get(fmt.Sprintf("http://%s/settings", ip))
	if err != nil {
		deviceName = deviceID // Fallback to device ID
	} else {
		defer func() {
			if err := settingsResp.Body.Close(); err != nil {
				log.Printf("Error closing response body: %v", err)
			}
		}()
		var settings ShellySettings
		if err := json.NewDecoder(settingsResp.Body).Decode(&settings); err != nil {
			deviceName = deviceID // Fallback to device ID
		} else {
			// Try to get device name from various possible locations
			if settings.Name != "" {
				deviceName = settings.Name
			} else if settings.Device.Name != "" {
				deviceName = settings.Device.Name
			} else if settings.Device.Hostname != "" {
				deviceName = settings.Device.Hostname
			} else if settings.Hostname != "" {
				deviceName = settings.Hostname
			} else {
				deviceName = deviceID // Final fallback
			}
		}
	}

	return &ShellyDevice{
		IP:         ip,
		DeviceID:   deviceID,
		DeviceName: deviceName,
		DeviceType: info.Type,
		LastSeen:   time.Now(),
	}
}

// collectMetricsFromKnownDevices collects metrics from all known Shelly devices
func (e *ShellyExporter) collectMetricsFromKnownDevices(ctx context.Context) {
	e.devicesMutex.RLock()
	devices := make([]*ShellyDevice, 0, len(e.knownDevices))
	for _, device := range e.knownDevices {
		devices = append(devices, device)
	}
	e.devicesMutex.RUnlock()

	if len(devices) == 0 {
		log.Printf("No known devices to collect metrics from")
		return
	}

	// log.Printf("Collecting metrics from %d known devices...", len(devices))
	start := time.Now()

	e.mutex.Lock()
	// Reset metrics
	e.powerGauge.Reset()
	e.mutex.Unlock()

	var wg sync.WaitGroup
	successCount := 0
	var successMutex sync.Mutex

	// Collect metrics from each known device
	for _, device := range devices {
		wg.Add(1)
		go func(dev *ShellyDevice) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			default:
			}

			if e.collectShellyMetrics(dev.IP, dev.DeviceID, dev.DeviceName, dev.DeviceType) {
				successMutex.Lock()
				successCount++
				successMutex.Unlock()
			}
		}(device)
	}

	wg.Wait()

	duration := time.Since(start).Seconds()
	log.Printf("Metrics collection completed in %.2f seconds, collected from %d/%d devices", duration, successCount, len(devices))
}

// getIPRange returns a list of IP addresses in the local network range
func (e *ShellyExporter) getIPRange() []string {
	var ips []string

	// Parse the network range (assuming CIDR notation like 192.168.1.0/24)
	_, ipNet, err := net.ParseCIDR(e.networkRange)
	if err != nil {
		log.Printf("Error parsing network range %s: %v", e.networkRange, err)
		return ips
	}

	// Generate IP addresses in the range
	for ip := ipNet.IP.Mask(ipNet.Mask); ipNet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast addresses
	if len(ips) > 2 {
		return ips[1 : len(ips)-1]
	}
	return ips
}

// inc increments an IP address
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// collectShellyMetrics collects metrics from a Shelly device using known device info
func (e *ShellyExporter) collectShellyMetrics(ip, deviceID, deviceName, deviceType string) bool {
	client := &http.Client{Timeout: 5 * time.Second}

	// Get device status
	statusResp, err := client.Get(fmt.Sprintf("http://%s/status", ip))
	if err != nil {
		log.Printf("Error getting status from %s: %v", ip, err)
		return false
	}
	defer func() {
		if err := statusResp.Body.Close(); err != nil {
			log.Printf("Error closing response body: %v", err)
		}
	}()

	var status ShellyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		log.Printf("Error decoding status from %s: %v", ip, err)
		return false
	}

	e.mutex.Lock()
	defer e.mutex.Unlock()

	// Set power metrics for each meter
	for _, meter := range status.Meters {
		if meter.IsValid {
			e.powerGauge.WithLabelValues(
				deviceID,
				deviceName,
				deviceType,
				ip,
			).Set(meter.Power)
		}
	}

	// log.Printf("Collected metrics from Shelly device %s ('%s', %s) at %s", deviceID, deviceName, deviceType, ip)
	return true
}

// startPeriodicDiscovery starts the periodic device discovery
func (e *ShellyExporter) startPeriodicDiscovery(ctx context.Context) {
	// Initial discovery
	e.discoverDevices(ctx)

	ticker := time.NewTicker(e.discoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.discoverDevices(ctx)
		}
	}
}

// startPeriodicMetricsCollection starts the periodic metrics collection from known devices
func (e *ShellyExporter) startPeriodicMetricsCollection(ctx context.Context) {
	// Wait a bit for initial discovery to complete
	time.Sleep(5 * time.Second)

	// Initial metrics collection
	e.collectMetricsFromKnownDevices(ctx)

	ticker := time.NewTicker(e.metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.collectMetricsFromKnownDevices(ctx)
		}
	}
}

// getEnv gets an environment variable with a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	// Configuration - can be overridden by environment variables
	networkRange := getEnv("NETWORK_RANGE", "10.10.10.0/24")
	discoveryIntervalStr := getEnv("DISCOVERY_INTERVAL", "60s")
	metricsIntervalStr := getEnv("METRICS_INTERVAL", "10s")
	port := getEnv("HTTP_PORT", ":8080")

	// Parse intervals
	discoveryInterval, err := time.ParseDuration(discoveryIntervalStr)
	if err != nil {
		log.Fatalf("Invalid discovery interval '%s': %v", discoveryIntervalStr, err)
	}

	metricsInterval, err := time.ParseDuration(metricsIntervalStr)
	if err != nil {
		log.Fatalf("Invalid metrics interval '%s': %v", metricsIntervalStr, err)
	}

	// Ensure port starts with ':'
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	log.Printf("Starting Shelly Prometheus Exporter")
	log.Printf("Network range: %s", networkRange)
	log.Printf("Device discovery interval: %s", discoveryInterval)
	log.Printf("Metrics collection interval: %s", metricsInterval)
	log.Printf("Metrics endpoint: http://localhost%s/metrics", port)

	// Create exporter
	exporter := NewShellyExporter(networkRange, discoveryInterval, metricsInterval)

	// Register with Prometheus
	prometheus.MustRegister(exporter)

	// Start periodic processes in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go exporter.startPeriodicDiscovery(ctx)
	go exporter.startPeriodicMetricsCollection(ctx)

	// Setup HTTP server for metrics
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := fmt.Fprintf(w, `
<html>
<head><title>Shelly Prometheus Exporter</title></head>
<body>
<h1>Shelly Prometheus Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
<p>Network range: %s</p>
<p>Device discovery interval: %s</p>
<p>Metrics collection interval: %s</p>
</body>
</html>`, networkRange, discoveryInterval, metricsInterval); err != nil {
			log.Printf("Error writing HTTP response: %v", err)
		}
	})

	log.Printf("Starting HTTP server on %s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}
