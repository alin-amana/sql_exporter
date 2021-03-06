package sql_exporter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/free/sql_exporter/config"
	"github.com/golang/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Exporter is a prometheus.Gatherer that gathers SQL metrics from targets and merges them with the default registry.
type Exporter interface {
	prometheus.Gatherer

	Config() *config.Config
}

type exporter struct {
	config          *config.Config
	jobs            []Job
	targets         []Target
	defaultGatherer prometheus.Gatherer
}

// NewExporter returns a new SQL Exporter for the provided config.
func NewExporter(configFile string, defaultGatherer prometheus.Gatherer) (Exporter, error) {
	c, err := config.Load(configFile)
	if err != nil {
		return nil, err
	}

	jobs := make([]Job, 0, len(c.Jobs))
	targets := make([]Target, 0, len(c.Jobs)*3)
	for _, jc := range c.Jobs {
		job, err := NewJob(jc)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
		targets = append(targets, job.Targets()...)
	}

	return &exporter{
		config:          c,
		jobs:            jobs,
		targets:         targets,
		defaultGatherer: defaultGatherer,
	}, nil
}

// Gather implements prometheus.Gatherer.
func (e *exporter) Gather() ([]*dto.MetricFamily, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(e.config.Globals.ScrapeTimeout))
	// Make sure to cancel the context, releasing any resources associated with it.
	defer cancel()

	var (
		metricChan = make(chan Metric, capMetricChan)
		errs       prometheus.MultiError
	)

	var wg sync.WaitGroup
	wg.Add(len(e.targets))
	for _, t := range e.targets {
		go func(target Target) {
			defer wg.Done()
			target.Collect(ctx, metricChan)
		}(t)
	}

	// Wait for all collectors to complete, then close the channel.
	go func() {
		wg.Wait()
		close(metricChan)
	}()

	// Drain metricChan in case of premature return.
	defer func() {
		for range metricChan {
		}
	}()

	// Gather.
	dtoMetricFamilies := make(map[string]*dto.MetricFamily, 10)
	for metric := range metricChan {
		dtoMetric := &dto.Metric{}
		if err := metric.Write(dtoMetric); err != nil {
			errs = append(errs, err)
			continue
		}
		metricDesc := metric.Desc()
		dtoMetricFamily, ok := dtoMetricFamilies[metricDesc.Name()]
		if !ok {
			dtoMetricFamily = &dto.MetricFamily{}
			dtoMetricFamily.Name = proto.String(metricDesc.Name())
			dtoMetricFamily.Help = proto.String(metricDesc.Help())
			switch {
			case dtoMetric.Gauge != nil:
				dtoMetricFamily.Type = dto.MetricType_GAUGE.Enum()
			case dtoMetric.Counter != nil:
				dtoMetricFamily.Type = dto.MetricType_COUNTER.Enum()
			default:
				errs = append(errs, fmt.Errorf("don't know how to handle metric %v", dtoMetric))
				continue
			}
			dtoMetricFamilies[metricDesc.Name()] = dtoMetricFamily
		}
		dtoMetricFamily.Metric = append(dtoMetricFamily.Metric, dtoMetric)
	}

	// No need to sort metric families, prometheus.Gatherers will do that for us when merging.
	result := make([]*dto.MetricFamily, 0, len(dtoMetricFamilies))
	for _, mf := range dtoMetricFamilies {
		result = append(result, mf)
	}
	return result, errs
}

// Config implements Exporter.
func (e *exporter) Config() *config.Config {
	return e.config
}
