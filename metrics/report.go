package metrics

import (
	"runtime"
	"sync/atomic"
	"time"
)

var (
	// ResponseTime reports the response time of handlers and rpc
	ResponseTime = "response_time_ns"
	// ConnectedClients represents the number of current connected clients in frontend servers
	ConnectedClients = "connected_clients"
	// ProcessDelay reports the message processing delay to handle the messages at the handler service
	ProcessDelay = "handler_delay_ns"
	// Goroutines reports the number of goroutines
	Goroutines = "goroutines"
	// HeapSize reports the size of heap
	HeapSize = "heapsize"
	// HeapObjects reports the number of allocated heap objects
	HeapObjects = "heapobjects"
	// ExceededRateLimiting reports the number of requests made in a connection
	// after the rate limit was exceeded
	ExceededRateLimiting = "exceeded_rate_limiting"

	//MetricsStartTime = "metrics_start_time"

	MessageCount = "metrics_message_count"
	messageCount = int32(0)
)

type Reporter interface {
	ReportCount(metric string, tags map[string]string, count float64) error
	ReportSummary(metric string, tags map[string]string, value float64) error
	ReportGauge(metric string, tags map[string]string, value float64) error
}

func CountMessage() {
	atomic.AddInt32(&messageCount, 1)
}

func ReportTiming(start int64, reporters []Reporter, route string) {
	if len(reporters) > 0 {
		elapsed := time.Since(time.Unix(0, start))
		tags := map[string]string{
			"route": route,
		}
		for _, r := range reporters {
			r.ReportSummary(ResponseTime, tags, float64(elapsed.Nanoseconds()))
		}
	}
}

func ReportMessageProcessDelay(start int64, reporters []Reporter, route string) {
	if len(reporters) > 0 {
		elapsed := time.Since(time.Unix(0, start))
		tags := map[string]string{
			"route": route,
		}
		for _, r := range reporters {
			r.ReportSummary(ProcessDelay, tags, float64(elapsed.Nanoseconds()))
		}
	}
}

func ReportNumberOfConnectedClients(reporters []Reporter, number int64) {
	for _, r := range reporters {
		r.ReportGauge(ConnectedClients, map[string]string{}, float64(number))
	}
}

func ReportSysMetrics(reporters []Reporter, period time.Duration) {
	for {
		for _, r := range reporters {
			num := runtime.NumGoroutine()
			m := &runtime.MemStats{}
			runtime.ReadMemStats(m)

			r.ReportGauge(Goroutines, map[string]string{}, float64(num))
			r.ReportGauge(HeapSize, map[string]string{}, float64(m.Alloc))
			r.ReportGauge(HeapObjects, map[string]string{}, float64(m.HeapObjects))
			r.ReportGauge(MessageCount, map[string]string{}, float64(atomic.LoadInt32(&messageCount)))
		}

		time.Sleep(period)
	}
}

func ReportExceededRateLimiting(reporters []Reporter) {
	for _, r := range reporters {
		r.ReportCount(ExceededRateLimiting, map[string]string{}, 1)
	}
}
