package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/VictoriaMetrics/metrics"
)

// Register various time series.
// Time series name may contain labels in Prometheus format - see below.
var (
	// Counter
	counters map[string]*metrics.Counter

	// // Register counter without labels.
	// requestsTotal = metrics.NewCounter("requests_total")
	// // Register summary with a single label.
	// requestDuration = metrics.NewSummary(`requests_duration_seconds{path="/foobar/baz"}`)
	// // Register gauge with two labels.
	// queueSize = metrics.NewGauge(`queue_size{queue="foobar",topic="baz"}`, func() float64 {
	// 	return float64(333)
	// })
	// // Register histogram with a single label.
	// responseSize = metrics.NewHistogram(`response_size{path="/foo/bar"}`)
)

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

type c5Response struct {
	ProxyState    string        // "proxyState" : "active",
	BuildVersion  string        // "buildVersion": "Version: 6.0.2.57, compiled on Jan 15 2020, 13:06:31 built by TELES Communication Systems GmbH",
	StartupTime   string        // "startupTime" : "2020-01-19 04:01:04.503",
	MemoryUsage   string        // "memoryUsage" : "C5 Heap Health: OK  - Mem used: 2%  - Mem used: 57MB  - Mem total: 2048MB  - Max: 3% - UpdCtr: 13198",
	TuQueueStatus string        // "tuQueueStatus" : "OK - checked: 1830",
	CounterInfos  []interface{} // "counterInfos": [ ... ]
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

func getCounterMetric(name string) *metrics.Counter {
	c, ok := counters[name]
	if !ok {
		c = metrics.NewCounter(name)

		counters[name] = c
	}
	return c
}

func setUsageMetric(prefix string, metric usageCounter) {
	log.Println("set usage metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, metric.Name+"_current", metric.Idx)
	setMetricValue(current, metric.Current)
	lastMin := buildMetricName(prefix, metric.Name+"_lastmin", metric.Idx)
	setMetricValue(lastMin, metric.LastMax)
	lastAvg := buildMetricName(prefix, metric.Name+"_lastavg", metric.Idx)
	setMetricValue(lastAvg, metric.LastMax)
	lastMax := buildMetricName(prefix, metric.Name+"_lastmax", metric.Idx)
	setMetricValue(lastMax, metric.LastMax)
}

func setCounterMetric(prefix string, metric eventCounter) {
	log.Println("set usage metric for ", prefix, metric.Name)
	current := buildMetricName(prefix, metric.Name+"_total", metric.Idx)
	setMetricValue(current, metric.Total)
}

func setMetricValue(name string, value uint64) {
	log.Println("set metric ", name, "value", value)
	metrics.GetOrCreateCounter(name).Set(value)

}

func parseInt64(str string) int64 {
	// log.Printf("Attempting to parse string as int64: '%s'", str)
	i64, err := strconv.ParseInt(str, 10, 63)
	if err != nil {
		log.Fatal("Failed to parse as int64:", str)
	}
	return i64
}

func parseUint64(str string) uint64 {
	return uint64(parseInt64(str))
}

func parseUsageCounter(line string) usageCounter {
	// "       Usage counters                              current    min    max   lMin   lMax   lAvg",
	// " 45 CALL_CONTROL_ACTIVE_CALLS                           0      0      0      0      0      0",
	parts := strings.Fields(line)
	return usageCounter{
		ID:      parts[0],
		Name:    parts[1],
		Current: parseUint64(parts[2]),
		LastMin: parseUint64(parts[5]),
		LastAvg: parseUint64(parts[6]),
		LastMax: parseUint64(parts[7]),
	}
}

func parseSubUsageCounter(lines []string) (cnts []usageCounter) {
	// [
	//   " 84 TRANSACTION_AND_TU_TU_MANAGER_QUEUE_SIZE          0      0      3      0      9      0",
	//   "                                                      0      0      3      0      4      0",
	//   "                                                      0      0      2      0      3      0",
	// ]
	// Name must be derived from first line, additional index must be added
	name := ""
	id := ""
	for i, line := range lines {
		idx := i
		if i == 0 {
			c := parseUsageCounter(line)
			c.Idx = &idx
			name = c.Name
			id = c.ID
			cnts = append(cnts, c)
		} else {
			parts := strings.Fields(line)
			cnts = append(cnts,
				usageCounter{
					ID:      id,
					Name:    name,
					Idx:     &idx,
					Current: parseUint64(parts[0]),
					LastMin: parseUint64(parts[3]),
					LastAvg: parseUint64(parts[4]),
					LastMax: parseUint64(parts[5]),
				})
		}
	}
	return
}

func parseEventCounter(line string) eventCounter {
	// "       Event counters                              absolute   curr   last",
	// "  0 TRANSPORT_MESSAGE_IN                              6461     31     69",
	parts := strings.Fields(line)
	return eventCounter{
		ID:    parts[0],
		Name:  parts[1],
		Total: parseUint64(parts[2]),
	}
}

func processC5Counter(prefix string, lines []interface{}) string {
	isGauge := false
	for _, line := range lines {
		v := reflect.ValueOf(line)
		// log.Println("Processing line of type", v.Type(), "kind", v.Kind())
		switch v.Kind() {
		case reflect.Slice, reflect.Array:
			//log.Println("Detected array")
			sublines := make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				sublines[i] = v.Index(i).Elem().String()
			}
			counter := parseSubUsageCounter(sublines)
			for _, c := range counter {
				setUsageMetric(prefix, c)
			}
		case reflect.String:
			// log.Println("Detected string")
			if strings.Contains(line.(string), "Event counters") {
				isGauge = false
				continue
			} else if strings.Contains(line.(string), "Usage counters") {
				isGauge = true
				continue
			}
			if isGauge {
				c := parseUsageCounter(line.(string))
				setUsageMetric(prefix, c)
			} else {
				c := parseEventCounter(line.(string))
				setCounterMetric(prefix, c)
				// log.Println("gauge #", c.ID, "name", c.Name, "value", c.Total)
			}
			// log.Println("line", line, "isGauge", isGauge)
		}

	}
	return ""
}

func fetchMetrics(prefix, url string) {
	resp, err := http.Get(url)
	if err != nil {
		log.Println("Failed to connect", err)
		return
		// panic(err)
	}
	defer resp.Body.Close()
	var c5state c5Response
	log.Println("Parsing response body", resp.Body)
	err = json.NewDecoder(resp.Body).Decode(&c5state)
	if err != nil {
		log.Println("Failed to parse, err: ", err)
	}
	processC5Counter(prefix, c5state.CounterInfos)
}

var sipproxydURL = "http://127.0.0.1:9980/c5/proxy/commands?49&1&-v"
var acdQueuedURL = "http://127.0.0.1:9982/c5/proxy/commands?49&1&-v"
var registrardURL = "http://127.0.0.1:9984/c5/proxy/commands?49&1&-v"

func main() {

	// Expose the registered metrics at `/metrics` path.
	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		fetchMetrics("sipproxyd", sipproxydURL)
		fetchMetrics("acdqueued", acdQueuedURL)
		fetchMetrics("registrard", registrardURL)
		metrics.WritePrometheus(w, true)
	})

	log.Println("Starting c5exporter on port 9055")
	log.Fatal(http.ListenAndServe(":9055", nil))
}

// Example response
// {
// 	"proxyResponseTimeStampAndState:" : "2020-01-19 11:40:01  active",
// 	"proxyState" : "active",
// 	"buildVersion:" : "Version: 6.0.2.57, compiled on Jan 15 2020, 13:06:31 built by TELES Communication Systems GmbH",
// 	"startupTime:" : "2020-01-19 04:01:04.503",
// 	"memoryUsage" : "C5 Heap Health: OK  - Mem used: 2%  - Mem used: 57MB  - Mem total: 2048MB  - Max: 3% - UpdCtr: 13198",
// 	"tuQueueStatus" : "OK - checked: 1830",
// 	"counterInfos" : [
// 	  "       Event counters                              absolute   curr   last",
// 	  "  0 TRANSPORT_MESSAGE_IN                              6502      0     72",
// 	  "  1 TRANSPORT_MESSAGE_OUT                             7088      0     79",
// 	  "253 TRANSPORT_TCP_MESSAGE_IN                             0      0      0",
// 	  "254 TRANSPORT_TCP_MESSAGE_OUT                            0      0      0",
// 	  "  2 REQUEST_METHOD_INVITE_IN                             0      0      0",
// 	  "  6 REQUEST_METHOD_SUBSCRIBE_IN                        334      0      5",
// 	  " 30 REQUEST_METHOD_NOOP_IN                            4964      0     54",
// 	  "  9 REQUEST_METHOD_NOTIFY_OUT                           39      0      0",
// 	  " 52 CALL_CONTROL_ORIG_CALL_SETUP_SUCCESS                 0      0      0",
// 	  " 54 CALL_CONTROL_ORIG_CALL_FAST_CONNECTED                0      0      0",
// 	  " 53 CALL_CONTROL_ORIG_CALL_CONNECTED                     0      0      0",
// 	  " 47 CALL_CONTROL_ORIG_CLIENT_ERROR                       0      0      0",
// 	  " 48 CALL_CONTROL_ORIG_SERVER_ERROR                       0      0      0",
// 	  " 49 CALL_CONTROL_ORIG_GLOBAL_ERROR                       0      0      0",
// 	  " 50 CALL_CONTROL_ORIG_REDIRECTION                        0      0      0",
// 	  " 51 CALL_CONTROL_ORIG_AUTHENTICATION_REQUIRED            0      0      0",
// 	  "190 OVERLOAD_PROTECTION_LIMIT_REACHED                    0      0      0",
// 	  "214 OVERLOAD_HEAP_WARNING_REJECTED_IN_REQUESTS           0      0      0",
// 	  "215 OVERLOAD_HEAP_CRITICAL_REJECTED_IN_REQUESTS          0      0      0",
// 	  "191 OVERLOAD_LIMIT1_REJECTED_IN_REQUESTS                 0      0      0",
// 	  "192 OVERLOAD_LIMIT2_REJECTED_IN_REQUESTS                 0      0      0",
// 	  "193 OVERLOAD_LIMIT3_REJECTED_IN_REQUESTS                 0      0      0",
// 	  "194 OVERLOAD_LIMIT4_REJECTED_IN_REQUESTS                 0      0      0",
// 	  "367 CALLS_LIMIT_REACHED                                  0      0      0",
// 	  "368 BT_CALLS_LIMIT_REACHED                               0      0      0",
// 	  "369 USER_CALLS_LIMIT_REACHED                             0      0      0",
// 	  " 46 CALL_CONTROL_AUTHENTICATION_ERROR                    0      0      0",
// 	  "227 CALL_CONTROL_IN_ACL_DENY                             0      0      0",
// 	  "228 CALL_CONTROL_OUT_ACL_DENY                            0      0      0",
// 	  "329 IP_FILTER_DENIED                                     0      0      0",
// 	  "330 IP_FILTER_NOT_ALLOWED                                0      0      0",
// 	  " 76 PRESENCE_AUTHENTICATION_ERROR                        0      0      0",
// 	  " 77 TRANSACTION_AND_TU_RETRY_IN                         50      0      0",
// 	  " 78 TRANSACTION_AND_TU_RETRY_OUT                        46      0      0",
// 	  " 83 TRANSACTION_AND_TU_CONN_VERIFICATION_RELEASED        0      0      0",
// 	  " 93 LOCATION_DNS_RESOLVER_ERROR                          0      0      0",
// 	  " 95 LOCATION_DNS_QUERY_TIMEOUT                           0      0      0",
// 	  "129 DATABASE_ERRORS                                      6      0      0",
// 	  "366 DATABASE_NOSQL_ERRORS                                0      0      0",
// 	  "144 ROUTING_ERRORS                                       0      0      0",
// 	  "177 SNMP_REQUESTS                                      908      0     10",
// 	  "178 SNMP_TRAPS                                           5      0      0",
// 	  "267 GENERAL_RCC_IN_COMMANDS                              3      0      0",
// 	  "268 GENERAL_RCC_OUT_COMMANDS                             3      0      0",
// 	  "350 WS_AGENT_EV_IN                                       0      0      0",
// 	  "351 WS_AGENT_EV_OUT                                      0      0      0",
// 	  "352 WS_CALL_EV                                           0      0      0",
// 	  "360 WS_CALL_SYNC_IN                                      0      0      0",
// 	  "359 WS_CALL_SYNC_OUT                                     0      0      0",
// 	  "362 WS_CALL_NOTIFY_IN                                    0      0      0",
// 	  "361 WS_CALL_NOTIFY_OUT                                   0      0      0",
// 	  "379 PUSH_CALL_NOTIFY                                     0      0      0",
// 	  "380 PUSH_CALL_NOTIFY_ERROR                               0      0      0",
// 	  "       Usage counters                              current    min    max   lMin   lMax   lAvg",
// 	  " 45 CALL_CONTROL_ACTIVE_CALLS                           0      0      0      0      0      0",
// 	  "309 BT_ACTIVE_CALLS                                     0      0      0      0      0      0",
// 	  " 75 PRESENCE_ACTIVE_SUBSCRIPTIONS                       6      6      6      6      6      6",
// 	  " 82 TRANSACTION_AND_TU_ACTIVE_SESSIONS                  0      0      0      0      0      0",
// 	  "189 TRANSACTION_AND_TU_ACTIVE_UA_SESSIONS               0      0      0      0      0      0",
// 	  " 81 TRANSACTION_AND_TU_ACTIVE_TRANSACTION_USERS         0      0      0      0      2      0",
// 	  "322 TRANSACTION_AND_TU_ACTIVE_INVITE_SERVER             0      0      0      0      0      0",
// 	  "233 TRANSPORT_TCP_ACTIVE_IN_CONNECTION                  0      0      0      0      0      0",
// 	  "234 TRANSPORT_TCP_ACTIVE_TRUSTED_IN_CONNECTION          0      0      0      0      0      0",
// 	  "235 TRANSPORT_TCP_ACTIVE_OUT_CONNECTION                 0      0      0      0      0      0",
// 	  "236 TRANSPORT_TCP_ACTIVE_TRUSTED_OUT_CONNECTION         0      0      0      0      0      0",
// 	  "264 GENERAL_RCC_ACTIVE_CONNECTIONS                      0      0      0      0      0      0",
// 	  "349 WS_CONNECTIONS                                      6      6      6      6      6      6",
// 	  [
// 		" 84 TRANSACTION_AND_TU_TU_MANAGER_QUEUE_SIZE          0      0      0      0      0      0",
// 		"                                                      0      0      0      0      0      0",
// 		"                                                      0      0      0      0      1      0",
// 		"                                                      0      0      0      0      0      0",
// 		"                                                      0      0      0      0      1      0"
// 	  ]
// 	]
// }