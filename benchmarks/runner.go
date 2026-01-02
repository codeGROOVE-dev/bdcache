//go:build ignore

// runner.go runs gocachemark benchmarks and validates results.
//
// Usage:
//
//	go run benchmarks/runner.go                  # solo fido, validate hitrate
//	go run benchmarks/runner.go -competitive    # gold medalists, track rankings
//
// Environment variables:
//
//	TESTS=cdn,meta        # filter to specific tests
//	SUITES=hitrate        # filter to specific suites
//	SIZES=16,32           # filter to specific cache sizes (in thousands)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// hitrateGoals are the minimum acceptable averages across all cache sizes.
// Keys must match gocachemark JSON output (camelCase).
var hitrateGoals = map[string]float64{
	"cdn":          58.3,
	"meta":         72.0,
	"twitter":      84.5,
	"wikipedia":    33.042,
	"thesiosBlock": 24.785,
	"thesiosFile":  93.458,
	"ibmDocker":    83.33,
	"tencentPhoto": 20.891,
}

// hitRateKeys maps display names to JSON keys for hit rate lookup.
var hitRateKeys = map[string]string{
	"CDN":           "cdn",
	"Meta":          "meta",
	"Zipf":          "zipf",
	"Twitter":       "twitter",
	"Wikipedia":     "wikipedia",
	"Thesios Block": "thesiosBlock",
	"Thesios File":  "thesiosFile",
	"IBM Docker":    "ibmDocker",
	"Tencent Photo": "tencentPhoto",
}

// goldMedalists are the caches to compare in competitive mode.
var goldMedalists = "fido,otter,clock,theine,sieve,freelru-sync"

// suiteGoals are the minimum/maximum acceptable averages across all tests in each suite.
// Note: latency and throughput absolute values are hardware-dependent and validated via
// placement (relative to other caches) rather than absolute thresholds.
var suiteGoals = struct {
	minHitRate         float64 // minimum average hit rate %
	maxMemory          int     // maximum bytes per item
	maxLatencyPlace    int     // maximum placement in latency category (1 = 1st place)
	maxThroughputPlace int     // maximum placement in throughput category (1 = 1st place)
}{
	minHitRate:         61.61,
	maxMemory:          80,
	maxLatencyPlace:    1,
	maxThroughputPlace: 1,
}

const (
	minFidoScore    = 154
	gocachemarkRepo = "github.com/tstromberg/gocachemark"
	fidoModule      = "github.com/codeGROOVE-dev/fido"
)

func main() {
	competitive := flag.Bool("competitive", false, "Run competitive benchmark with gold medalists")
	flag.Parse()

	// Find fido root (where we're running from).
	fidoDir, err := findFidoDir()
	if err != nil {
		fatal("finding fido directory: %v", err)
	}

	// Find or clone gocachemark.
	gocachemarkDir, err := findOrCloneGocachemark(fidoDir)
	if err != nil {
		fatal("finding gocachemark: %v", err)
	}

	// Update go.mod replace directive.
	cmd := exec.Command("go", "mod", "edit", "-replace", fidoModule+"="+fidoDir)
	cmd.Dir = gocachemarkDir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("updating go.mod replace: %v", err)
	}

	// Prepare output directory for results.
	benchmarksDir := filepath.Join(fidoDir, "benchmarks")

	// Load reference results for comparison.
	ref, _ := loadResults(filepath.Join(benchmarksDir, "gocachemark_results.json"))

	// Build gocachemark arguments.
	args := []string{"run", "."}
	if *competitive {
		args = append(args, "-caches", goldMedalists)
	} else {
		args = append(args, "-caches", "fido")
	}

	// Always use temp directory for output first.
	outdir, err := os.MkdirTemp("", "gocachemark-")
	if err != nil {
		fatal("creating temp directory: %v", err)
	}
	defer os.RemoveAll(outdir)
	args = append(args, "-outdir", outdir)

	// Add filters if env vars are set.
	testsFilter := os.Getenv("TESTS")
	suitesFilter := os.Getenv("SUITES")
	sizesFilter := os.Getenv("SIZES")
	if testsFilter != "" {
		args = append(args, "-tests", testsFilter)
	}
	if suitesFilter != "" {
		args = append(args, "-suites", suitesFilter)
	}
	if sizesFilter != "" {
		args = append(args, "-sizes", sizesFilter)
	}

	// Track whether this is a full run (no filters).
	fullRun := testsFilter == "" && suitesFilter == "" && sizesFilter == ""

	// Run gocachemark with streaming output.
	mode := "fido"
	if *competitive {
		mode = "competitive"
	}
	fmt.Printf("Running %s benchmarks via gocachemark...\n\n", mode)
	results, err := runGocachemark(gocachemarkDir, args, outdir)
	if err != nil {
		fatal("running gocachemark: %v", err)
	}

	// Show deltas against reference.
	fmt.Println()
	if ref != nil {
		showDeltas(ref, results)
	}

	// Validate results.
	if err := validateHitrate(results); err != nil {
		fatal("%v", err)
	}
	if err := validateSuiteGoals(results); err != nil {
		fatal("%v", err)
	}
	if *competitive {
		if err := validateCompetitive(results, ref, testsFilter, suitesFilter); err != nil {
			fatal("%v", err)
		}
		// Only save results if all tests were run (no filters).
		if fullRun {
			if err := copyResults(outdir, benchmarksDir); err != nil {
				fatal("saving results: %v", err)
			}
			fmt.Printf("\nResults saved to %s/\n", benchmarksDir)
		} else {
			fmt.Printf("\nResults NOT saved (filtered run: TESTS=%q SUITES=%q SIZES=%q)\n", testsFilter, suitesFilter, sizesFilter)
		}
	}
}

func findFidoDir() (string, error) {
	// Look for go.mod with fido module.
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		modPath := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			if strings.Contains(string(data), fidoModule) {
				return dir, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("could not find fido root (no go.mod with %s)", fidoModule)
}

func findOrCloneGocachemark(fidoDir string) (string, error) {
	// Check locations in order of preference.
	locations := []string{
		os.Getenv("GOCACHEMARK_DIR"),
		filepath.Join(os.Getenv("HOME"), "src", "gocachemark"),
		filepath.Join(fidoDir, "out", "gocachemark"),
	}

	for _, loc := range locations {
		if loc == "" {
			continue
		}
		if isGocachemarkDir(loc) {
			return loc, nil
		}
	}

	// Clone to out/gocachemark.
	cloneDir := filepath.Join(fidoDir, "out", "gocachemark")
	fmt.Printf("Cloning gocachemark to %s...\n", cloneDir)

	if err := os.MkdirAll(filepath.Dir(cloneDir), 0755); err != nil {
		return "", fmt.Errorf("creating out directory: %w", err)
	}

	cmd := exec.Command("git", "clone", "https://"+gocachemarkRepo, cloneDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cloning gocachemark: %w", err)
	}

	return cloneDir, nil
}

func isGocachemarkDir(dir string) bool {
	mainGo := filepath.Join(dir, "main.go")
	if _, err := os.Stat(mainGo); err != nil {
		return false
	}
	goMod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(goMod)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), gocachemarkRepo)
}

func runGocachemark(dir string, args []string, outdir string) (*Results, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Stream output to stdout.
	s := bufio.NewScanner(stdout)
	for s.Scan() {
		fmt.Println(s.Text())
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("reading output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	return loadResults(filepath.Join(outdir, "gocachemark_results.json"))
}

// Results represents gocachemark JSON output.
type Results struct {
	HitRate    map[string]json.RawMessage `json:"hitRate"`
	Latency    map[string]json.RawMessage `json:"latency"`
	Throughput map[string]json.RawMessage `json:"throughput"`
	Memory     *MemoryResults             `json:"memory"`
	Rankings   []RankEntry                `json:"rankings"`
	MedalTable MedalTable                 `json:"medalTable"`
}

type MemoryResults struct {
	Results  []MemoryEntry `json:"results"`
	Capacity int           `json:"capacity"`
	ValSize  int           `json:"valSize"`
}

type MemoryEntry struct {
	Name         string `json:"name"`
	Items        int    `json:"items"`
	Bytes        int64  `json:"bytes"`
	BytesPerItem int    `json:"bytesPerItem"`
}

type MedalTable struct {
	Categories []Category `json:"categories"`
}

type Category struct {
	Name       string      `json:"name"`
	Benchmarks []Benchmark `json:"benchmarks"`
	Rankings   []RankEntry `json:"rankings"`
}

type Benchmark struct {
	Name   string   `json:"name"`
	Gold   []string `json:"gold"`
	Silver []string `json:"silver"`
	Bronze []string `json:"bronze"`
}

type CacheResult struct {
	Name    string  `json:"name"`
	AvgRate float64 `json:"avgRate"`
}

type LatencyResult struct {
	Name    string  `json:"name"`
	AvgNsOp float64 `json:"avgNsOp"`
}

type ThroughputResult struct {
	Name   string  `json:"name"`
	AvgQps float64 `json:"avgQps"`
}

type RankEntry struct {
	Rank   int    `json:"rank"`
	Name   string `json:"name"`
	Score  int    `json:"score"`
	Gold   int    `json:"gold"`
	Silver int    `json:"silver"`
	Bronze int    `json:"bronze"`
}

type placement struct {
	medal string
	value float64
}

// hitRateResults extracts cache results for a test, skipping non-test fields like "sizes".
func (r *Results) hitRateResults(name string) ([]CacheResult, error) {
	raw, ok := r.HitRate[name]
	if !ok {
		return nil, fmt.Errorf("test %q not found", name)
	}
	var out []CacheResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func loadResults(path string) (*Results, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results Results
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, err
	}
	return &results, nil
}

func validateHitrate(res *Results) error {
	fmt.Println("=== Hitrate Validation ===")

	var fails []string
	for name, goal := range hitrateGoals {
		caches, err := res.hitRateResults(name)
		if err != nil {
			fmt.Printf("? %s: %v\n", name, err)
			continue
		}

		var avg float64
		var found bool
		for _, c := range caches {
			if c.Name == "fido" {
				avg = c.AvgRate
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("? %s: fido not found\n", name)
			continue
		}

		// Use tiny tolerance for floating point comparison.
		if avg >= goal-0.000001 {
			fmt.Printf("✓ %s: %.2f%% (goal: %.2f%%)\n", name, avg, goal)
		} else {
			fmt.Printf("✗ %s: %.2f%% (goal: %.2f%%)\n", name, avg, goal)
			fails = append(fails, fmt.Sprintf("%s: %.2f%% < %.2f%%", name, avg, goal))
		}
	}

	if len(fails) > 0 {
		return fmt.Errorf("hitrate goals not met:\n  %s", strings.Join(fails, "\n  "))
	}
	fmt.Println("\nAll hitrate goals met!")
	return nil
}

func validateSuiteGoals(res *Results) error {
	fmt.Println("\n=== Suite Goals Validation ===")
	var fails []string

	// Hit rate average.
	var hitRates []float64
	for name := range res.HitRate {
		if name == "sizes" {
			continue
		}
		caches, err := res.hitRateResults(name)
		if err != nil {
			continue
		}
		if rate := findHitRate(caches, "fido"); rate > 0 {
			hitRates = append(hitRates, rate)
		}
	}
	if len(hitRates) > 0 {
		avg := sum(hitRates) / float64(len(hitRates))
		if avg >= suiteGoals.minHitRate {
			fmt.Printf("✓ hitrate avg: %.2f%% (goal: ≥%.2f%%)\n", avg, suiteGoals.minHitRate)
		} else {
			fmt.Printf("✗ hitrate avg: %.2f%% (goal: ≥%.2f%%)\n", avg, suiteGoals.minHitRate)
			fails = append(fails, fmt.Sprintf("hitrate avg %.2f%% < %.2f%%", avg, suiteGoals.minHitRate))
		}
	}

	// Note: Latency and throughput absolute values are hardware-dependent.
	// Validation is done via placement (relative ranking) in validateCompetitive.

	// Memory.
	if res.Memory != nil {
		if bytes := findMemory(res.Memory.Results, "fido"); bytes > 0 {
			if bytes <= suiteGoals.maxMemory {
				fmt.Printf("✓ memory: %d bytes/item (goal: ≤%d)\n", bytes, suiteGoals.maxMemory)
			} else {
				fmt.Printf("✗ memory: %d bytes/item (goal: ≤%d)\n", bytes, suiteGoals.maxMemory)
				fails = append(fails, fmt.Sprintf("memory %d > %d", bytes, suiteGoals.maxMemory))
			}
		}
	}

	if len(fails) > 0 {
		return fmt.Errorf("suite goals not met:\n  %s", strings.Join(fails, "\n  "))
	}
	return nil
}

func sum(vals []float64) float64 {
	var total float64
	for _, v := range vals {
		total += v
	}
	return total
}

func showDeltas(ref, curr *Results) {
	fmt.Println("=== Deltas vs Reference ===")
	var any bool

	// Hit rate deltas (higher is better).
	for name := range curr.HitRate {
		if name == "sizes" {
			continue
		}
		refCaches, err := ref.hitRateResults(name)
		if err != nil {
			continue
		}
		currCaches, err := curr.hitRateResults(name)
		if err != nil {
			continue
		}
		refVal := findHitRate(refCaches, "fido")
		currVal := findHitRate(currCaches, "fido")
		if refVal == 0 {
			continue
		}
		delta := currVal - refVal
		pct := delta / refVal * 100
		any = true
		fmt.Printf("  hitrate/%s: %.2f%% → %.2f%% (%+.2f, %+.1f%%)\n", name, refVal, currVal, delta, pct)
	}

	// Latency deltas (lower is better).
	for name := range curr.Latency {
		var refResults, currResults []LatencyResult
		if raw, ok := ref.Latency[name]; ok {
			json.Unmarshal(raw, &refResults)
		}
		if raw, ok := curr.Latency[name]; ok {
			json.Unmarshal(raw, &currResults)
		}
		refVal := findLatency(refResults, "fido")
		currVal := findLatency(currResults, "fido")
		if refVal == 0 {
			continue
		}
		delta := currVal - refVal
		pct := delta / refVal * 100
		any = true
		// Negative delta is good for latency
		fmt.Printf("  latency/%s: %.1fns → %.1fns (%+.1f, %+.1f%%)\n", name, refVal, currVal, delta, pct)
	}

	// Throughput deltas (higher is better).
	for name := range curr.Throughput {
		if name == "threads" {
			continue
		}
		var refResults, currResults []ThroughputResult
		if raw, ok := ref.Throughput[name]; ok {
			json.Unmarshal(raw, &refResults)
		}
		if raw, ok := curr.Throughput[name]; ok {
			json.Unmarshal(raw, &currResults)
		}
		refVal := findThroughput(refResults, "fido")
		currVal := findThroughput(currResults, "fido")
		if refVal == 0 {
			continue
		}
		delta := currVal - refVal
		pct := delta / refVal * 100
		any = true
		fmt.Printf("  throughput/%s: %.2fM → %.2fM (%+.2fM, %+.1f%%)\n", name, refVal/1e6, currVal/1e6, delta/1e6, pct)
	}

	// Memory delta (lower is better).
	if ref.Memory != nil && curr.Memory != nil {
		refVal := findMemory(ref.Memory.Results, "fido")
		currVal := findMemory(curr.Memory.Results, "fido")
		if refVal > 0 && currVal > 0 {
			delta := currVal - refVal
			pct := float64(delta) / float64(refVal) * 100
			any = true
			fmt.Printf("  memory/bytesPerItem: %d → %d (%+d, %+.1f%%)\n", refVal, currVal, delta, pct)
		}
	}

	if !any {
		fmt.Println("  (no reference data)")
	}
	fmt.Println()
}

func findHitRate(results []CacheResult, name string) float64 {
	for _, r := range results {
		if r.Name == name {
			return r.AvgRate
		}
	}
	return 0
}

func findLatency(results []LatencyResult, name string) float64 {
	for _, r := range results {
		if r.Name == name {
			return r.AvgNsOp
		}
	}
	return 0
}

func findThroughput(results []ThroughputResult, name string) float64 {
	for _, r := range results {
		if r.Name == name {
			return r.AvgQps
		}
	}
	return 0
}

func findMemory(results []MemoryEntry, name string) int {
	for _, r := range results {
		if r.Name == name {
			return r.BytesPerItem
		}
	}
	return 0
}

func validateCompetitive(res, prev *Results, testsFilter, suitesFilter string) error {
	// Find fido in rankings.
	var mc *RankEntry
	for i := range res.Rankings {
		if res.Rankings[i].Name == "fido" {
			mc = &res.Rankings[i]
			break
		}
	}
	if mc == nil {
		return fmt.Errorf("fido not found in rankings")
	}

	if prev != nil {
		fmt.Println("=== Ranking Changes ===")
		reportChanges(prev, res)
	}

	fullRun := testsFilter == "" && suitesFilter == ""

	// Only show final validation for full runs or when all tests in a suite were run.
	if !fullRun && testsFilter != "" {
		// Partial test run - skip validation entirely.
		return nil
	}

	// Check which suites were run (empty means all).
	suiteSet := make(map[string]bool)
	if suitesFilter != "" {
		for _, s := range strings.Split(suitesFilter, ",") {
			suiteSet[strings.ToLower(strings.TrimSpace(s))] = true
		}
	}

	// For partial suite runs, only validate relevant placements.
	var fails []string
	headerPrinted := false

	// Score validation only for full runs.
	if fullRun {
		fmt.Println("\n=== Final Validation ===")
		headerPrinted = true
		if mc.Score >= minFidoScore {
			fmt.Printf("✓ fido score: %d (goal: ≥%d)\n", mc.Score, minFidoScore)
		} else {
			fmt.Printf("✗ fido score: %d (goal: ≥%d)\n", mc.Score, minFidoScore)
			fails = append(fails, fmt.Sprintf("score %d < %d", mc.Score, minFidoScore))
		}

		if prev != nil {
			var prevScore int
			for _, r := range prev.Rankings {
				if r.Name == "fido" {
					prevScore = r.Score
					break
				}
			}
			if mc.Score >= prevScore {
				fmt.Printf("✓ No point reduction (was %d, now %d)\n", prevScore, mc.Score)
			} else {
				fmt.Printf("⚠ Point reduction: %d → %d\n", prevScore, mc.Score)
			}
		}
	}

	// Validate category placements.
	for _, cat := range res.MedalTable.Categories {
		var maxPlace int
		suiteName := strings.ToLower(cat.Name)
		switch cat.Name {
		case "Latency":
			maxPlace = suiteGoals.maxLatencyPlace
		case "Throughput":
			maxPlace = suiteGoals.maxThroughputPlace
		default:
			continue
		}

		// Skip if this suite wasn't included in a filtered run.
		if len(suiteSet) > 0 && !suiteSet[suiteName] {
			continue
		}

		for _, r := range cat.Rankings {
			if r.Name == "fido" {
				if !headerPrinted {
					fmt.Println("\n=== Final Validation ===")
					headerPrinted = true
				}
				if r.Rank <= maxPlace {
					fmt.Printf("✓ %s placement: %d (goal: ≤%d)\n", cat.Name, r.Rank, maxPlace)
				} else {
					fmt.Printf("✗ %s placement: %d (goal: ≤%d)\n", cat.Name, r.Rank, maxPlace)
					fails = append(fails, fmt.Sprintf("%s placement %d > %d", cat.Name, r.Rank, maxPlace))
				}
				break
			}
		}
	}

	if len(fails) > 0 {
		return fmt.Errorf("competitive validation failed:\n  %s", strings.Join(fails, "\n  "))
	}
	return nil
}

func reportChanges(prev, curr *Results) {
	prevP := buildPlacementMap(prev)
	currP := buildPlacementMap(curr)

	prevR := make(map[string]RankEntry)
	for _, r := range prev.Rankings {
		prevR[r.Name] = r
	}

	for _, r := range curr.Rankings {
		p, ok := prevR[r.Name]
		if !ok {
			fmt.Printf("%s: new entry with %d points\n", r.Name, r.Score)
			continue
		}

		delta := r.Score - p.Score
		if delta == 0 {
			continue
		}

		sign := "+"
		if delta < 0 {
			sign = ""
		}
		fmt.Printf("%s: %d → %d (%s%d points)\n", r.Name, p.Score, r.Score, sign, delta)

		// Show which benchmarks changed for this cache.
		for bench, cp := range currP[r.Name] {
			pp := prevP[r.Name][bench]
			if cp.medal == pp.medal {
				continue
			}
			pm, cm := pp.medal, cp.medal
			if pm == "" {
				pm = "none"
			}
			if cm == "" {
				cm = "none"
			}
			if cp.value != 0 && pp.value != 0 {
				fmt.Printf("  %s: %s → %s (%.2f%% → %.2f%%)\n", bench, pm, cm, pp.value, cp.value)
			} else {
				fmt.Printf("  %s: %s → %s\n", bench, pm, cm)
			}
		}
	}
}

func buildPlacementMap(r *Results) map[string]map[string]placement {
	out := make(map[string]map[string]placement)
	for _, rank := range r.Rankings {
		out[rank.Name] = make(map[string]placement)
	}

	for _, cat := range r.MedalTable.Categories {
		for _, b := range cat.Benchmarks {
			name := cat.Name + "/" + b.Name

			// Get hit rate values if applicable.
			var vals map[string]float64
			if cat.Name == "Hit Rate" {
				if key, ok := hitRateKeys[b.Name]; ok {
					if caches, err := r.hitRateResults(key); err == nil {
						vals = make(map[string]float64)
						for _, c := range caches {
							vals[c.Name] = c.AvgRate
						}
					}
				}
			}

			for cache := range out {
				var medal string
				switch {
				case slices.Contains(b.Gold, cache):
					medal = "gold"
				case slices.Contains(b.Silver, cache):
					medal = "silver"
				case slices.Contains(b.Bronze, cache):
					medal = "bronze"
				}
				var v float64
				if vals != nil {
					v = vals[cache]
				}
				out[cache][name] = placement{medal, v}
			}
		}
	}
	return out
}

func copyResults(src, dst string) error {
	files := []string{"gocachemark_results.json", "gocachemark_results.md"}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
