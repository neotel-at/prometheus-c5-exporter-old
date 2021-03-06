package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.neotel.at/neotel/c5-exporter/config"
	"github.com/VictoriaMetrics/metrics"
	"github.com/jinzhu/configor"
)

const version = "1.0.1"

// Global metric set
var metricSet *metrics.Set

type eventCounter struct {
	ID    string
	Name  string
	Idx   *int
	Total uint64
}

type usageCounter struct {
	ID      string
	Name    string
	Idx     *int
	Current uint64
	LastMin uint64
	LastAvg uint64
	LastMax uint64
}

type c5StateResponse struct {
	ProxyState       string        // "proxyState" : "active", // sipproxyd only
	QueueState       string        // "queueState" : "active", // acdqueued only
	RegistrarState   string        // "registrarState" : "active", // registar only
	BuildVersion     string        // "buildVersion": "Version: 6.0.2.57, compiled on Jan 15 2020, 13:06:31 built by TELES Communication Systems GmbH",
	BuildVersionOld  string        `json:"buildVersion:"` // Workaround for typo in "buildVersion:" (trailing colon) before R6.2
	StartupTime      string        // "startupTime" : "2020-01-19 04:01:04.503",
	StartupTimeOld   string        `json:"startupTime:"` // Workaround for typo in "startupTime:" (trailing colon) before R6.2
	MemoryUsage      string        // "memoryUsage" : "C5 Heap Health: OK  - Mem used: 2%  - Mem used: 57MB  - Mem total: 2048MB  - Max: 3% - UpdCtr: 13198",
	TuQueueStatus    string        // "tuQueueStatus" : "OK - checked: 1830",
	CounterInfos     []interface{} // "counterInfos": [ ... ]
	AlarmedTrapInfos []interface{} // "alarmedTrapInfos": [ ... ]
}

type c5CounterResponse struct {
	ProxyResponseTimeStampAndState string        // "proxyResponseTimeStampAndState:" : "2021-02-25 10:31:48  active",
	CounterName                    string        // "counterName" : "BT_CALLS_LIMIT_REACHED",
	CounterType                    string        // "counterType" : "EVENT",
	AbsoluteValue                  uint64        // "absoluteValue" : 0, // event only
	CurrentValue                   uint64        // "currentValue" : 0, // event only
	LastValue                      uint64        // "lastValue" : 0, // event only
	MinValue                       uint64        // "minValue" : 0, // counter only
	MaxValue                       uint64        // "maxValue" : 0, // counter only
	LastMinValue                   uint64        // "lastMinValue" : 0, // counter only
	LastMaxValue                   uint64        // "lastMaxValue" : 0, // counter only
	LastAvgValue                   uint64        // "lastAvgValue" : 0, // counter only
	TotalValue                     uint64        // "totalValue" : 0, // counter only
	TableValues                    []interface{} // "counterInfos": [ ... ]
}

func buildMetricName(prefix string, name string, idx *int) string {
	if prefix != "" {
		name = prefix + "_" + name
	}
	name = strings.ToLower(name)
	if idx != nil {
		return fmt.Sprintf(`%s{idx="%d"}`, name, *idx)
	}
	return name
}

func normalizeMetricName(name string) string {
	// Avoid unwanted trailing chars like in
	// v6.0.2.69: TRANSACTION_AND_TU_TU_MANAGER_REINJECT_QUEUE_
	return strings.Trim(name, "_. ")
}

func setUsageMetric(prefix string, metric usageCounter) {
	// logDebug("set usage metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, metric.Name+"_current", metric.Idx)
	setMetricValue(current, metric.Current)
	lastMin := buildMetricName(prefix, metric.Name+"_lastmin", metric.Idx)
	setMetricValue(lastMin, metric.LastMin)
	lastAvg := buildMetricName(prefix, metric.Name+"_lastavg", metric.Idx)
	setMetricValue(lastAvg, metric.LastAvg)
	lastMax := buildMetricName(prefix, metric.Name+"_lastmax", metric.Idx)
	setMetricValue(lastMax, metric.LastMax)
}

func setLabeledUsageMetric(prefix string, label string, metric usageCounter) {
	// logDebug("set labeled usage metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, `current{`+label+`="`+metric.Name+`"}`, metric.Idx)
	setMetricValue(current, metric.Current)
	lastMin := buildMetricName(prefix, `lastmin{`+label+`="`+metric.Name+`"}`, metric.Idx)
	setMetricValue(lastMin, metric.LastMin)
	lastAvg := buildMetricName(prefix, `lastavg{`+label+`="`+metric.Name+`"}`, metric.Idx)
	setMetricValue(lastAvg, metric.LastAvg)
	lastMax := buildMetricName(prefix, `lastmax{`+label+`="`+metric.Name+`"}`, metric.Idx)
	setMetricValue(lastMax, metric.LastMax)
}

func setCounterMetric(prefix string, metric eventCounter) {
	// logDebug("set counter metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, metric.Name+"_total", metric.Idx)
	setMetricValue(current, metric.Total)
}

func setLabeledCounterMetric(prefix string, label string, metric eventCounter) {
	// logDebug("set labeled counter metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, `total{`+label+`="`+metric.Name+`"}`, metric.Idx)
	setMetricValue(current, metric.Total)
}

func setMetricValue(name string, value uint64) {
	// logDebug("set metric ", name, "value", value)
	metricSet.GetOrCreateCounter(name).Set(value)
}

func parseInt64(str string) int64 {
	// logDebug("Attempting to parse string as int64: '%s'", str)
	i64, err := strconv.ParseInt(str, 10, 63)
	if err != nil {
		log.Fatal("Failed to parse as int64:", str)
	}
	return i64
}

func parseUint64(str string) uint64 {
	return uint64(parseInt64(str))
}

func parseBuildString(build string) (version string) {
	// "Version: 6.0.2.57, compiled on Jan 15 2020, 13:06:31 built by TELES Communication Systems GmbH",
	parts := strings.Split(build, ",")
	version = strings.TrimPrefix(parts[0], "Version: ")
	return
}

func parseDataSize(str string) uint64 {
	unit := strings.TrimLeft(str, "0123456789")
	size := parseUint64(strings.TrimSuffix(str, unit))
	switch strings.ToLower(unit) {
	case "kb":
		return size * 1024
	case "mb":
		return size * 1024 * 1024
	case "gb":
		return size * 1024 * 1024 * 1024
	case "tb":
		return size * 1024 * 1024 * 1024 * 1024
	}
	return size
}

func parseMemoryString(memoryUsage string) (memUsed, memTotal, memMaxUsage uint64) {
	// R6.0: "memoryUsage" : "C5 Heap Health: OK  - Mem used: 18%  - Mem used: 383MB  - Mem total: 2048MB  - Max: 18% - UpdCtr: 60793",
	// R6.2: "memoryUsage" : "C5 Heap Health: OK  - Mem used: 3%  76MB  (min: 76 max: 76)  - Mem total: 2048MB  - MAX: 3% - UpdCtr: 92205",
	parts := strings.Split(memoryUsage, "-")
	for _, p := range parts {
		param := strings.SplitN(strings.TrimSpace(p), ":", 2)
		// logDebug("Parsing memory part", p, param)
		key := strings.ToLower(strings.TrimSpace(param[0]))
		switch key {
		case "mem used":
			if strings.HasSuffix(param[1], "%") { // Need the first "mem used" as percent param
				continue
			}
			if strings.Contains(param[1], "%") { // probably R6.2
				// logDebug("Parse memused R6.2", param[1])
				memparts := strings.Fields(param[1])
				memUsed = parseDataSize(memparts[1])
			} else {
				// logDebug("Parse memused R6.0", param[1])
				memUsed = parseDataSize(strings.TrimSpace(param[1]))
			}
		case "mem total":
			memTotal = parseDataSize(strings.TrimSpace(param[1]))
		case "max":
			memMaxUsage = parseUint64(strings.TrimSuffix(strings.TrimSpace(param[1]), "%"))
		}
	}
	return
}

func parseMemoryStringRegex(memoryUsage string) (memUsed, memTotal, memMaxUsage uint64) {
	// R6.0: "memoryUsage" : "C5 Heap Health: OK  - Mem used: 18%  - Mem used: 383MB  - Mem total: 2048MB  - Max: 18% - UpdCtr: 60793",
	// R6.2: "memoryUsage" : "C5 Heap Health: OK  - Mem used: 3%  76MB  (min: 76 max: 76)  - Mem total: 2048MB  - MAX: 3% - UpdCtr: 92205",
	memRegex := regexp.MustCompile(`(?i)mem used:(?: *\d+%)? *(\d+[tgmkb]*) .* mem total: *(\d+[tgmkb]*).* max: *(\d+)%`)
	matches := memRegex.FindStringSubmatch(memoryUsage)
	if len(matches) > 1 {
		// logDebug("matches:", matches[1:4])
		return parseDataSize(matches[1]), parseDataSize(matches[2]), parseUint64(matches[3])
	}
	logError("Failed to parse memory usage:", memoryUsage)
	return
}

func parseProcessStateString(state ...string) uint64 {
	for _, s := range state {
		// Skip the state only if empty
		if s != "" {
			// Otherwise return active=1, inactive=0 or
			// 2 for all other unknown states
			switch s {
			case "active":
				return 1
			case "inactive", "passive":
				return 0
			}
			return 2
		}
	}
	return 3
}

func parseQueueStateString(state string) uint64 {
	if strings.HasPrefix(state, "OK") {
		return 1
	}
	return 0
}

func parseUsageCounter(line string) usageCounter {
	// "       Usage counters                              current    min    max   lMin   lMax   lAvg",
	// " 45 CALL_CONTROL_ACTIVE_CALLS                           0      0      0      0      0      0",
	parts := strings.Fields(line)
	if len(parts) < 8 {
		return usageCounter{}
	}
	return usageCounter{
		ID:      parts[0],
		Name:    normalizeMetricName(parts[1]),
		Current: parseUint64(parts[2]),
		LastMin: parseUint64(parts[5]),
		LastMax: parseUint64(parts[6]),
		LastAvg: parseUint64(parts[7]),
	}
}

func parseSubUsageCounter(lines []string) (cnts []usageCounter) {
	// [
	//   " 84 TRANSACTION_AND_TU_TU_MANAGER_QUEUE_SIZE          0      0      3      0      9      0",
	//   "                                                      0      0      3      0      4      0",
	// ]
	// Name must be derived from first line, additional index must be added
	name := ""
	id := ""
	for i, line := range lines {
		idx := i
		if i == 0 {
			c := parseUsageCounter(line)
			if c.Name == "" {
				logError("Failed to parse as sub usage counter header:", line)
				return
			}
			c.Idx = &idx
			name = c.Name
			id = c.ID
			cnts = append(cnts, c)
		} else {
			parts := strings.Fields(line)
			if len(parts) < 6 {
				logError("Failed to parse as sub usage counter:", line)
				continue
			}
			cnts = append(cnts,
				usageCounter{
					ID:      id,
					Name:    normalizeMetricName(name),
					Idx:     &idx,
					Current: parseUint64(parts[0]),
					LastMin: parseUint64(parts[3]),
					LastMax: parseUint64(parts[4]),
					LastAvg: parseUint64(parts[5]),
				})
		}
	}
	return
}

func parseEventCounter(line string) eventCounter {
	// "       Event counters                              absolute   curr   last",
	// "  0 TRANSPORT_MESSAGE_IN                              6461     31     69",
	parts := strings.Fields(line)
	if len(parts) < 3 {
		return eventCounter{}
	}
	return eventCounter{
		ID:    parts[0],
		Name:  normalizeMetricName(parts[1]),
		Total: parseUint64(parts[2]),
	}
}

func parseSubEventCounter(lines []string) (cnts []eventCounter) {
	// [
	//   "425 CASS_ERR_CONN_TMO                                  0      0      0",
	//   "                                                     131    386    518"
	// ],
	// Name must be derived from first line, additional index must be added
	name := ""
	id := ""
	for i, line := range lines {
		idx := i
		if i == 0 {
			c := parseEventCounter(line)
			if c.Name == "" {
				logError("Failed to parse as sub event counter header:", line)
				return
			}
			c.Idx = &idx
			name = c.Name
			id = c.ID
			cnts = append(cnts, c)
		} else {
			parts := strings.Fields(line)
			if len(parts) < 1 {
				logError("Failed to parse as sub event counter:", line)
				return
			}
			cnts = append(cnts,
				eventCounter{
					ID:    id,
					Name:  normalizeMetricName(name),
					Idx:   &idx,
					Total: parseUint64(parts[0]),
				})
		}
	}
	return
}
func processC5StateCounter(prefix string, lines []interface{}) {
	const event, usage string = "event", "usage"
	var cntType string
	for _, line := range lines {
		v := reflect.ValueOf(line)
		switch v.Kind() {
		case reflect.Slice, reflect.Array:
			sublines := make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				sublines[i] = v.Index(i).Elem().String()
			}
			if cntType == usage {
				cnts := parseSubUsageCounter(sublines)
				for _, c := range cnts {
					setUsageMetric(prefix, c)
				}
			} else if cntType == event {
				cnts := parseSubEventCounter(sublines)
				for _, c := range cnts {
					setCounterMetric(prefix, c)
				}
			} else {
				logDebug(prefix, "ignoring line for unknown type", sublines)
			}
		case reflect.String:
			l := line.(string)
			if strings.Contains(l, "Event counters") {
				cntType = event
				continue
			} else if strings.Contains(l, "Usage counters") {
				cntType = usage
				continue
			} else if strings.HasPrefix(l, "    ") {
				// Skip unknown elements like the OBSERVERS line:
				// " 75 PRESENCE_ACTIVE_SUBSCRIPTIONS                       36     36     36     36     36     36       2045",
				// "    OBSERVERS  (dialog,csta,reg):  36,0,0",
				logDebug(prefix, "ignore line", l)
				continue
			}
			if cntType == usage {
				c := parseUsageCounter(l)
				setUsageMetric(prefix, c)
			} else if cntType == event {
				c := parseEventCounter(l)
				setCounterMetric(prefix, c)
			} else {
				logDebug(prefix, "ignoring line", l)
			}
			// logDebug("line type", cntType, line)
		}
	}
	return
}

// processC5CounterMetrics will parse a counter output of type EVENT and USAGE for
// a specific C5 metric.
//
// {
//   "proxyResponseTimeStampAndState:" : "2021-02-25 10:31:48  active",
//   "counterName" : "BT_CALLS_LIMIT_REACHED",
//   "counterType" : "EVENT",
//   "absoluteValue" : 0,
//   "currentValue" : 0,
//   "lastValue" : 0,
//   "tableValues" : [
//     "name                            absolute   curr   last",
//     "trunkname1.ipcentrex.internal         0      0      0",
//     "trunk2.otherprovider.at               0      0      0",
//   ],
//   "tableCountInfo" : "curComponentCount2: 14 (10000) "
// }
func processC5CounterMetrics(basePrefix string, data c5CounterResponse) {
	const event, usage string = "EVENT", "USAGE"
	prefix := basePrefix + "_" + strings.ToLower(data.CounterName)
	setMetricValue(prefix+`_current`, data.CurrentValue)
	logDebug("Processing", prefix, "type", data.CounterType)
	if data.CounterType == event {
		setMetricValue(prefix+`_total`, data.AbsoluteValue)
		setMetricValue(prefix+`_last`, data.LastValue)
	} else {
		// setMetricValue(prefix+`_current_min`, data.MinValue)
		// setMetricValue(prefix+`_current_max`, data.MaxValue)
		setMetricValue(prefix+`_lastavg`, data.LastAvgValue)
		setMetricValue(prefix+`_lastmin`, data.LastMinValue)
		setMetricValue(prefix+`_lastmax`, data.LastMaxValue)
	}
	// Parse values now
	for _, line := range data.TableValues {
		v := reflect.ValueOf(line)
		switch v.Kind() {
		case reflect.String:
			l := line.(string)
			if strings.HasPrefix(l, "name") {
				continue
			}
			if data.CounterType == usage {
				c := parseUsageCounter("0 " + l)
				setLabeledUsageMetric(prefix+"_trunk", "name", c)
			} else if data.CounterType == event {
				c := parseEventCounter("0 " + l)
				setLabeledCounterMetric(prefix+"_trunk", "name", c)
			} else {
				logDebug(prefix, "ignoring line", l)
			}
		}
	}
	return
}

func clearMetrics(prefix string) {
	logDebug("Clear metric counters for", prefix)
	for _, name := range metricSet.ListMetricNames() {
		if strings.HasPrefix(name, prefix) {
			logDebug("Unregister metric counter", name)
			metricSet.UnregisterMetric(name)
		}
	}
}

func processBaseMetrics(prefix string, state c5StateResponse) {
	// Set build version in info string
	version := parseBuildString(state.BuildVersion)
	if version == "" { // Workaround for typo in sessionconsole before R6.2
		version = parseBuildString(state.BuildVersionOld)
	}
	startupTime := state.StartupTime
	if startupTime == "" { // Workaround for typo in sessionconsole before R6.2
		startupTime = state.StartupTimeOld
	}
	logInfo("Processed", prefix, version, "started", startupTime)
	setMetricValue(prefix+`_info{version="`+version+`",starttime="`+startupTime+`"}`, 1)

	// Set process/queue states (usually active=1 or inactive=0)
	setMetricValue(prefix+`_state`, parseProcessStateString(state.ProxyState, state.QueueState, state.RegistrarState))
	setMetricValue(prefix+`_tu_queue_state`, parseQueueStateString(state.TuQueueStatus))

	// Set process state (usually active=1 or inactive=0)
	memUsed, memTotal, memMaxUsage := parseMemoryString(state.MemoryUsage)
	setMetricValue(prefix+`_memory_used_bytes`, memUsed)
	setMetricValue(prefix+`_memory_total_bytes`, memTotal)
	setMetricValue(prefix+`_memory_max_used_percent`, memMaxUsage)
}

func fetchC5StateMetrics(prefix, url string, wg *sync.WaitGroup) {
	defer wg.Done()
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		logError("Failed to connect", err)
		clearMetrics(prefix)
		return
	}
	defer resp.Body.Close()
	var c5state c5StateResponse
	// logDebug("Parsing response body", resp.Body)
	err = json.NewDecoder(resp.Body).Decode(&c5state)
	if err != nil {
		logError("Failed to parse response, err: ", err)
		clearMetrics(prefix)
		return
	}
	// process base information
	processBaseMetrics(prefix, c5state)

	// process event and usage counters now
	processC5StateCounter(prefix, c5state.CounterInfos)
}

func fetchC5CounterMetrics(prefix, url string, wg *sync.WaitGroup) {
	defer wg.Done()
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		logError("Failed to connect", err)
		clearMetrics(prefix)
		return
	}
	defer resp.Body.Close()
	var c5Resp c5CounterResponse
	// logDebug("Parsing response body", resp.Body)
	err = json.NewDecoder(resp.Body).Decode(&c5Resp)
	if err != nil {
		logError("Failed to parse response, err: ", err)
		clearMetrics(prefix)
		return
	}

	// process event and usage counters now
	processC5CounterMetrics(prefix, c5Resp)
}

func main() {

	conf := config.AppConfig

	// Define and parse commandline flags for initial configuration
	configFile := flag.String("config", "", "Configuration file to load")
	flag.BoolVar(&conf.Debug, "debug", false, "Enable debug")
	flag.StringVar(&conf.ListenAddress, "listen", ":9055", "Listen address")
	flag.Parse()

	if conf.Debug {
		logInfo("Enabled debug logging")
	}

	if configFile != nil && *configFile != "" {
		logInfo("Loading configuration", *configFile)
		err := configor.New(&configor.Config{Debug: conf.Debug}).Load(conf, *configFile)
		if err != nil {
			log.Fatal("Unable to load configuration", *configFile, err)
		}

		// Reparse commandline flags to override loaded config parameters
		flag.Parse()
	} else {
		logInfo("No configuration file used. Enabling querying of all C5 processes.")
		conf.SIPProxydEnabled = true
		conf.ACDQueuedEnabled = true
		conf.RegistrardEnabled = true
	}

	if !(conf.SIPProxydEnabled || conf.SIPProxydTrunksEnabled || conf.ACDQueuedEnabled || conf.RegistrardEnabled) {
		logError("No c5 processes enabled to query. Please enable at least on process in configuration.")
		log.Fatal("Aborting.")
	}

	logDebug("Using configuration:")
	logDebug("- debug", conf.Debug, "listenAddress", conf.ListenAddress)
	logDebug("- sipproxyd", conf.SIPProxydEnabled)
	logDebug("  url:", conf.SIPProxydURL)
	logDebug("- sipproxyd-trunk", conf.SIPProxydTrunksEnabled)
	logDebug("  url stats:", conf.SIPProxydTrunkStatsURL)
	logDebug("  url limit:", conf.SIPProxydTrunkLimitsURL)
	logDebug("- acdqueued", conf.ACDQueuedEnabled)
	logDebug("  url:", conf.ACDQueuedURL)
	logDebug("- registard", conf.RegistrardEnabled)
	logDebug("  url:", conf.RegistrardURL)

	metricSet = metrics.NewSet()

	// Expose the registered metrics at `/metrics` path.
	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		var wg sync.WaitGroup
		if conf.SIPProxydEnabled {
			wg.Add(1)
			go fetchC5StateMetrics("sipproxyd", conf.SIPProxydURL, &wg)
		}
		if conf.ACDQueuedEnabled {
			wg.Add(1)
			go fetchC5StateMetrics("acdqueued", conf.ACDQueuedURL, &wg)
		}
		if conf.RegistrardEnabled {
			wg.Add(1)
			go fetchC5StateMetrics("registrard", conf.RegistrardURL, &wg)
		}
		wg.Wait()
		// We need to ensure sequential processing, so wait between fetches
		if conf.SIPProxydTrunksEnabled {
			wg.Add(1)
			go fetchC5CounterMetrics("sipproxyd", conf.SIPProxydTrunkStatsURL, &wg)
			wg.Wait()
			wg.Add(1)
			go fetchC5CounterMetrics("sipproxyd", conf.SIPProxydTrunkLimitsURL, &wg)
			wg.Wait()
		}
		metricSet.WritePrometheus(w)
		metrics.WriteProcessMetrics(w)
	})

	// logInfo(fmt.Printf("Starting c5exporter v%s on port %s", version, conf.ListenAddress))
	logInfo("Starting c5exporter version", version, "on", conf.ListenAddress)
	log.Fatal(http.ListenAndServe(conf.ListenAddress, nil))
}

func logInfo(msg ...interface{}) {
	log.Print("[INFO] ", fmt.Sprintln(msg...))
}

func logDebug(msg ...interface{}) {
	if config.AppConfig.Debug {
		log.Print("[DEBUG] ", fmt.Sprintln(msg...))
	}
}

func logError(msg ...interface{}) {
	log.Print("[ERROR] ", fmt.Sprintln(msg...))
}
