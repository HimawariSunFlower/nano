package metrics

import (
	"fmt"
)

type ConsoleReporter struct {
}

func NewConsoleReporter() *ConsoleReporter {

	sr := &ConsoleReporter{}

	return sr
}

func (s *ConsoleReporter) ReportCount(metric string, tagsMap map[string]string, count float64) error {
	fmt.Printf("count\t%s:%f\n", metric, count)

	return nil
}

// ReportGauge sents the gauge value and reports to statsd
func (s *ConsoleReporter) ReportGauge(metric string, tagsMap map[string]string, value float64) error {
	fmt.Printf("gauge\t%s:%f\n", metric, value)

	return nil
}

// ReportSummary observes the summary value and reports to statsd
func (s *ConsoleReporter) ReportSummary(metric string, tagsMap map[string]string, value float64) error {
	fmt.Printf("summary\t%s:%f\n", metric, value)

	return nil
}
