// armbian-stats-web: a self-contained web dashboard for Armbian / Linux SBCs.
//
// Serves a single-page dashboard (dark, instrument-panel style) over HTTP
// and a JSON API at /api/stats. Everything - HTML, CSS, JS - is embedded
// into the binary at build time, so deploying to a board is just "copy the
// binary over and run it". No external dependencies, no CDN assets, works
// fully offline on a LAN.
//
// Build:
//
//	go build -o armbian-stats-web .
//
// Cross-compile for a 64-bit ARM board (most modern Armbian devices):
//
//	GOOS=linux GOARCH=arm64 go build -o armbian-stats-web .
//
// Cross-compile for a 32-bit ARMv7 board (older boards):
//
//	GOOS=linux GOARCH=arm GOARM=7 go build -o armbian-stats-web .
//
// Run:
//
//	./armbian-stats-web                  # listens on :8080
//	./armbian-stats-web -addr :9000       # custom port
//	./armbian-stats-web -interval 5       # sample every 5s instead of 2s
//
// Then open http://<board-ip>:8080 in a browser on the same network.
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

//go:embed static/index.html
var indexTemplateSrc string

var renderedIndexHTML []byte

// historyLen controls how many samples are kept for the scrolling
// CPU/memory trace charts (e.g. 120 samples at a 2s interval = 4 minutes).
const historyLen = 120

// ---------- JSON payload types ----------

type snapshot struct {
	Time           string     `json:"time"`
	Hostname       string     `json:"hostname"`
	Board          string     `json:"board,omitempty"`
	OS             string     `json:"os,omitempty"`
	ArmbianVersion string     `json:"armbian_version,omitempty"`
	ArmbianBoard   string     `json:"armbian_board,omitempty"`
	Kernel         string     `json:"kernel"`
	UptimeSeconds  int64      `json:"uptime_seconds"`
	Load1          float64    `json:"load1"`
	Load5          float64    `json:"load5"`
	Load15         float64    `json:"load15"`
	CPU            cpuStat    `json:"cpu"`
	Temps          []tempZone `json:"temps"`
	Memory         memStat    `json:"memory"`
	Swap           swapStat   `json:"swap"`
	Disk           diskStat   `json:"disk"`
	Network        []netAddr  `json:"network"`
	Docker         dockerStat `json:"docker"`
	CPUHistory     []float64  `json:"cpu_history"`
	MemHistory     []float64  `json:"mem_history"`
}

type cpuStat struct {
	OverallPct float64   `json:"overall_pct"`
	Cores      []float64 `json:"cores"`
}

type tempZone struct {
	Name    string  `json:"name"`
	Celsius float64 `json:"celsius"`
}

type memStat struct {
	TotalKB uint64  `json:"total_kb"`
	UsedKB  uint64  `json:"used_kb"`
	Pct     float64 `json:"pct"`
}

type swapStat struct {
	Enabled bool    `json:"enabled"`
	TotalKB uint64  `json:"total_kb"`
	UsedKB  uint64  `json:"used_kb"`
	Pct     float64 `json:"pct"`
}

type diskStat struct {
	Path    string  `json:"path"`
	TotalKB uint64  `json:"total_kb"`
	UsedKB  uint64  `json:"used_kb"`
	Pct     float64 `json:"pct"`
}

type netAddr struct {
	Iface string `json:"iface"`
	IP    string `json:"ip"`
}

type dockerStat struct {
	Available  bool        `json:"available"`
	Error      string      `json:"error,omitempty"`
	Containers []dockerCtr `json:"containers"`
}

type dockerCtr struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	Ports  string `json:"ports"`
}

// ---------- shared state, updated by the background collector ----------

var (
	mu         sync.RWMutex
	current    snapshot
	cpuHistory = make([]float64, 0, historyLen)
	memHistory = make([]float64, 0, historyLen)

	havePrevSample bool
	prevOverall    cpuSample
	prevCores      []cpuSample
)

// ---------- /proc & /sys readers (same approach as the CLI version) ----------

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseOSRelease() map[string]string {
	m := map[string]string{}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "=", 2)
		if len(parts) != 2 {
			continue
		}
		m[parts[0]] = strings.Trim(parts[1], `"`)
	}
	return m
}

func getArmbianInfo() map[string]string {
	m := map[string]string{}
	for _, p := range []string{"/etc/armbian-release", "/etc/armbian-image-release"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.SplitN(sc.Text(), "=", 2)
			if len(parts) == 2 {
				m[parts[0]] = strings.Trim(parts[1], `"`)
			}
		}
		f.Close()
	}
	return m
}

func getBoardModel() string {
	data, err := readFile("/proc/device-tree/model")
	if err != nil {
		return ""
	}
	return strings.TrimRight(data, "\x00\n")
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func getKernel() string {
	data, err := readFile("/proc/version")
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(data)
	if len(fields) >= 3 {
		return fields[2]
	}
	return strings.TrimSpace(data)
}

func getUptimeSeconds() int64 {
	data, err := readFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(data)
	if len(fields) == 0 {
		return 0
	}
	secs, _ := strconv.ParseFloat(fields[0], 64)
	return int64(secs)
}

func getLoadAvg() (float64, float64, float64) {
	data, err := readFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return 0, 0, 0
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	l5, _ := strconv.ParseFloat(fields[1], 64)
	l15, _ := strconv.ParseFloat(fields[2], 64)
	return l1, l5, l15
}

type cpuSample struct {
	idle  uint64
	total uint64
}

func readCPUSamples() (cpuSample, []cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, nil, err
	}
	defer f.Close()

	var overall cpuSample
	var perCore []cpuSample

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		var nums [10]uint64
		for i := 1; i < len(fields) && i-1 < 10; i++ {
			v, _ := strconv.ParseUint(fields[i], 10, 64)
			nums[i-1] = v
		}
		idle := nums[3] + nums[4] // idle + iowait
		total := nums[0] + nums[1] + nums[2] + idle + nums[5] + nums[6] + nums[7]
		s := cpuSample{idle: idle, total: total}
		if fields[0] == "cpu" {
			overall = s
		} else {
			perCore = append(perCore, s)
		}
	}
	return overall, perCore, nil
}

func cpuUsagePct(a, b cpuSample) float64 {
	idleDelta := float64(b.idle - a.idle)
	totalDelta := float64(b.total - a.total)
	if totalDelta <= 0 {
		return 0
	}
	pct := (1 - idleDelta/totalDelta) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func getThermalZones() []tempZone {
	var zones []tempZone
	matches, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	sort.Strings(matches)
	for _, zonePath := range matches {
		typeData, err := readFile(filepath.Join(zonePath, "type"))
		if err != nil {
			continue
		}
		tempData, err := readFile(filepath.Join(zonePath, "temp"))
		if err != nil {
			continue
		}
		raw, err := strconv.ParseFloat(strings.TrimSpace(tempData), 64)
		if err != nil {
			continue
		}
		zones = append(zones, tempZone{
			Name:    strings.TrimSpace(typeData),
			Celsius: raw / 1000.0,
		})
	}
	return zones
}

func getMemAndSwap() (memStat, swapStat) {
	var totalKB, availKB, swapTotalKB, swapFreeKB uint64
	f, err := os.Open("/proc/meminfo")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			key := strings.TrimSuffix(fields[0], ":")
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			switch key {
			case "MemTotal":
				totalKB = val
			case "MemAvailable":
				availKB = val
			case "SwapTotal":
				swapTotalKB = val
			case "SwapFree":
				swapFreeKB = val
			}
		}
	}
	usedKB := totalKB - availKB
	memPct := 0.0
	if totalKB > 0 {
		memPct = float64(usedKB) / float64(totalKB) * 100
	}
	mem := memStat{TotalKB: totalKB, UsedKB: usedKB, Pct: memPct}

	swap := swapStat{}
	if swapTotalKB > 0 {
		swapUsed := swapTotalKB - swapFreeKB
		swap.Enabled = true
		swap.TotalKB = swapTotalKB
		swap.UsedKB = swapUsed
		swap.Pct = float64(swapUsed) / float64(swapTotalKB) * 100
	}
	return mem, swap
}

func getDiskUsage(path string) diskStat {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskStat{Path: path}
	}
	total := stat.Blocks * uint64(stat.Bsize) / 1024
	free := stat.Bavail * uint64(stat.Bsize) / 1024
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = float64(used) / float64(total) * 100
	}
	return diskStat{Path: path, TotalKB: total, UsedKB: used, Pct: pct}
}

func getNetworkAddrs() []netAddr {
	var out []netAddr
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			out = append(out, netAddr{Iface: iface.Name, IP: ipnet.IP.String()})
		}
	}
	return out
}

func getDockerContainers() dockerStat {
	path, err := exec.LookPath("docker")
	if err != nil {
		return dockerStat{Available: false, Error: "docker not installed"}
	}
	cmd := exec.Command(path, "ps", "--format", "{{.Names}}|{{.Image}}|{{.Status}}|{{.Ports}}")
	out, err := cmd.Output()
	if err != nil {
		return dockerStat{Available: false, Error: "could not reach docker daemon"}
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return dockerStat{Available: true, Containers: []dockerCtr{}}
	}
	var containers []dockerCtr
	for _, line := range strings.Split(trimmed, "\n") {
		parts := strings.Split(line, "|")
		ctr := dockerCtr{}
		if len(parts) > 0 {
			ctr.Name = parts[0]
		}
		if len(parts) > 1 {
			ctr.Image = parts[1]
		}
		if len(parts) > 2 {
			ctr.Status = parts[2]
		}
		if len(parts) > 3 {
			ctr.Ports = parts[3]
		}
		containers = append(containers, ctr)
	}
	return dockerStat{Available: true, Containers: containers}
}

// ---------- background collector ----------

func collectOnce() {
	overall, cores, err := readCPUSamples()
	var overallPct float64
	var corePcts []float64
	if err == nil && havePrevSample {
		overallPct = cpuUsagePct(prevOverall, overall)
		for i := range cores {
			if i < len(prevCores) {
				corePcts = append(corePcts, cpuUsagePct(prevCores[i], cores[i]))
			}
		}
	}
	if err == nil {
		prevOverall = overall
		prevCores = cores
		havePrevSample = true
	}
	if corePcts == nil {
		corePcts = make([]float64, len(cores))
	}

	osRel := parseOSRelease()
	armbian := getArmbianInfo()
	mem, swap := getMemAndSwap()
	l1, l5, l15 := getLoadAvg()

	snap := snapshot{
		Time:           time.Now().Format(time.RFC3339),
		Hostname:       getHostname(),
		Board:          getBoardModel(),
		OS:             osRel["PRETTY_NAME"],
		ArmbianVersion: armbian["VERSION"],
		ArmbianBoard:   armbian["BOARD"],
		Kernel:         getKernel(),
		UptimeSeconds:  getUptimeSeconds(),
		Load1:          l1,
		Load5:          l5,
		Load15:         l15,
		CPU:            cpuStat{OverallPct: overallPct, Cores: corePcts},
		Temps:          getThermalZones(),
		Memory:         mem,
		Swap:           swap,
		Disk:           getDiskUsage("/"),
		Network:        getNetworkAddrs(),
		Docker:         getDockerContainers(),
	}

	mu.Lock()
	cpuHistory = append(cpuHistory, overallPct)
	if len(cpuHistory) > historyLen {
		cpuHistory = cpuHistory[len(cpuHistory)-historyLen:]
	}
	memHistory = append(memHistory, mem.Pct)
	if len(memHistory) > historyLen {
		memHistory = memHistory[len(memHistory)-historyLen:]
	}
	snap.CPUHistory = append([]float64(nil), cpuHistory...)
	snap.MemHistory = append([]float64(nil), memHistory...)
	current = snap
	mu.Unlock()
}

func startCollector(interval time.Duration) {
	collectOnce()
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			collectOnce()
		}
	}()
}

// ---------- HTTP handlers ----------

func statsHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	snap := current
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		log.Printf("encode stats: %v", err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(renderedIndexHTML)
}

func main() {
	addr := flag.String("addr", ":8080", "address to listen on, e.g. :8080 or 0.0.0.0:9000")
	interval := flag.Int("interval", 2, "sample/poll interval in seconds")
	flag.Parse()

	tmpl, err := template.New("index").Parse(indexTemplateSrc)
	if err != nil {
		log.Fatalf("parse embedded template: %v", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, struct{ IntervalMs int }{IntervalMs: *interval * 1000}); err != nil {
		log.Fatalf("render template: %v", err)
	}
	renderedIndexHTML = []byte(buf.String())

	startCollector(time.Duration(*interval) * time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/stats", statsHandler)

	fmt.Printf("armbian-stats-web listening on %s (sampling every %ds)\n", *addr, *interval)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
