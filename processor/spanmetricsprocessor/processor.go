// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spanmetricsprocessor // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/spanmetricsprocessor"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.opentelemetry.io/collector/translator/conventions"
	tracetranslator "go.opentelemetry.io/collector/translator/trace"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/spanmetricsprocessor/internal/cache"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/spanmetricsprocessor/keybuilder"
)

const (
	serviceNameKey             = conventions.AttributeServiceName
	instrumentationLibraryName = "spanmetricsprocessor"
	operationKey               = "operation"   // OpenTelemetry non-standard constant.
	spanKindKey                = "span.kind"   // OpenTelemetry non-standard constant.
	statusCodeKey              = "status.code" // OpenTelemetry non-standard constant.
	traceIDKey                 = "trace.id"
	spanIDKey                  = "span.id"

	defaultDimensionsCacheSize         = 1000
	defaultResourceAttributesCacheSize = 1000
)

var (
	maxDurationMs = math.MaxFloat64

	defaultLatencyHistogramBucketsMs = []float64{
		2, 4, 6, 8, 10, 50, 100, 200, 400, 800, 1000, 1400, 2000, 5000, 10_000, 15_000, maxDurationMs,
	}
)

type exemplarData struct {
	traceID pdata.TraceID
	value   float64
}

type aggregationMeta struct {
	serviceName  string
	instrLibName instrLibKey
	resourceAttr pdata.AttributeMap
}

// metricKey is used to carry the stringified metric attributes
type metricKey string

// resourceKey is used to carry the stringified resource attributes
type resourceKey string

// instrLibKey is used to carry the stringified instrumentation library name
type instrLibKey string

type processorImp struct {
	// ConsumeTraces() of each instance might be called concurrently from its upstream component in the pipeline.
	// As this processor is stateful. Due to the nature of its logic, the concurrent executions of ConsumeTraces() will
	// output incorrect data. This lock forces the ConsumeTraces() execute in synchronization.
	lock   sync.Mutex
	logger *zap.Logger
	config Config

	metricsExporter component.MetricsExporter
	nextConsumer    consumer.Traces

	// Additional dimensions to add to metrics.
	dimensions []Dimension

	// Additional resourceAttributes to add to metrics.
	resourceAttributes []Dimension

	// The starting time of the data points.
	startTime time.Time

	// Call & Error counts.
	callSum map[resourceKey]map[instrLibKey]map[metricKey]int64

	// Latency histogram.
	latencyCount         map[resourceKey]map[instrLibKey]map[metricKey]uint64
	latencySum           map[resourceKey]map[instrLibKey]map[metricKey]float64
	latencyBucketCounts  map[resourceKey]map[instrLibKey]map[metricKey][]uint64
	latencyBounds        []float64
	latencyExemplarsData map[resourceKey]map[instrLibKey]map[metricKey][]exemplarData

	// An LRU cache of dimension key-value maps keyed by a unique identifier formed by a concatenation of its values:
	// e.g. { "foo/barOK": { "serviceName": "foo", "operation": "/bar", "status_code": "OK" }}
	metricKeyToDimensions *cache.Cache
	// An LRU cache of resourceattributekey-value maps keyed by a unique identifier formed by a concatenation of its values.
	resourceKeyToDimensions *cache.Cache

	// Defines whether metrics generated from spans should attach span and trace id as dimensions.
	attachSpanAndTraceID bool

	// Defines whether metrics should inherit instrumentation library name from span
	inheritInstrumentationLibraryName bool
}

func newProcessor(logger *zap.Logger, config config.Processor, nextConsumer consumer.Traces) (*processorImp, error) {
	logger.Info("Building spanmetricsprocessor")
	pConfig := config.(*Config)

	bounds := defaultLatencyHistogramBucketsMs
	if pConfig.LatencyHistogramBuckets != nil {
		bounds = mapDurationsToMillis(pConfig.LatencyHistogramBuckets)

		// "Catch-all" bucket always appended
		bounds = append(bounds, maxDurationMs)
	}

	if err := validateDimensions(pConfig.Dimensions, []string{spanKindKey, statusCodeKey}); err != nil {
		return nil, err
	}
	if err := validateDimensions(pConfig.ResourceAttributes, []string{serviceNameKey}); err != nil {
		return nil, err
	}

	metricKeyToDimensionsCache, err := cache.NewCache(pConfig.DimensionsCacheSize)
	if err != nil {
		return nil, err
	}

	resourceKeyToDimensionsCache, err := cache.NewCache(pConfig.ResourceAttributesCacheSize)
	if err != nil {
		return nil, err
	}

	return &processorImp{
		logger:                            logger,
		config:                            *pConfig,
		startTime:                         time.Now(),
		callSum:                           make(map[resourceKey]map[instrLibKey]map[metricKey]int64),
		latencyBounds:                     bounds,
		latencySum:                        make(map[resourceKey]map[instrLibKey]map[metricKey]float64),
		latencyCount:                      make(map[resourceKey]map[instrLibKey]map[metricKey]uint64),
		latencyBucketCounts:               make(map[resourceKey]map[instrLibKey]map[metricKey][]uint64),
		latencyExemplarsData:              make(map[resourceKey]map[instrLibKey]map[metricKey][]exemplarData),
		nextConsumer:                      nextConsumer,
		dimensions:                        pConfig.Dimensions,
		resourceAttributes:                pConfig.ResourceAttributes,
		resourceKeyToDimensions:           resourceKeyToDimensionsCache,
		metricKeyToDimensions:             metricKeyToDimensionsCache,
		attachSpanAndTraceID:              pConfig.AttachSpanAndTraceID,
		inheritInstrumentationLibraryName: pConfig.InheritInstrumentationLibraryName,
	}, nil
}

// durationToMillis converts the given duration to the number of milliseconds it represents.
// Note that this can return sub-millisecond (i.e. < 1ms) values as well.
func durationToMillis(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond.Nanoseconds())
}

func mapDurationsToMillis(vs []time.Duration) []float64 {
	vsm := make([]float64, len(vs))
	for i, v := range vs {
		vsm[i] = durationToMillis(v)
	}
	return vsm
}

// validateDimensions checks duplicates for reserved dimensions and additional dimensions. Considering
// the usage of Prometheus related exporters, we also validate the dimensions after sanitization.
func validateDimensions(dimensions []Dimension, defaults []string) error {
	labelNames := make(map[string]struct{})
	for _, d := range defaults {
		labelNames[d] = struct{}{}
		labelNames[sanitize(d)] = struct{}{}
	}
	labelNames[operationKey] = struct{}{}

	for _, key := range dimensions {
		if _, ok := labelNames[key.Name]; ok {
			return fmt.Errorf("duplicate dimension name %s", key.Name)
		}
		labelNames[key.Name] = struct{}{}

		sanitizedName := sanitize(key.Name)
		if sanitizedName == key.Name {
			continue
		}
		if _, ok := labelNames[sanitizedName]; ok {
			return fmt.Errorf("duplicate dimension name %s after sanitization", sanitizedName)
		}
		labelNames[sanitizedName] = struct{}{}
	}

	return nil
}

// Start implements the component.Component interface.
func (p *processorImp) Start(ctx context.Context, host component.Host) error {
	p.logger.Info("Starting spanmetricsprocessor")
	exporters := host.GetExporters()

	var availableMetricsExporters []string

	// The available list of exporters come from any configured metrics pipelines' exporters.
	for k, exp := range exporters[config.MetricsDataType] {
		metricsExp, ok := exp.(component.MetricsExporter)
		if !ok {
			return fmt.Errorf("the exporter %q isn't a metrics exporter", k.String())
		}

		availableMetricsExporters = append(availableMetricsExporters, k.String())

		p.logger.Debug("Looking for spanmetrics exporter from available exporters",
			zap.String("spanmetrics-exporter", p.config.MetricsExporter),
			zap.Any("available-exporters", availableMetricsExporters),
		)
		if k.String() == p.config.MetricsExporter {
			p.metricsExporter = metricsExp
			p.logger.Info("Found exporter", zap.String("spanmetrics-exporter", p.config.MetricsExporter))
			break
		}
	}
	if p.metricsExporter == nil {
		return fmt.Errorf("failed to find metrics exporter: '%s'; please configure metrics_exporter from one of: %+v",
			p.config.MetricsExporter, availableMetricsExporters)
	}
	p.logger.Info("Started spanmetricsprocessor")
	return nil
}

// Shutdown implements the component.Component interface.
func (p *processorImp) Shutdown(ctx context.Context) error {
	p.logger.Info("Shutting down spanmetricsprocessor")
	return nil
}

// Capabilities implements the consumer interface.
func (p *processorImp) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeTraces implements the consumer.Traces interface.
// It aggregates the trace data to generate metrics, forwarding these metrics to the discovered metrics exporter.
// The original input trace data will be forwarded to the next consumer, unmodified.
func (p *processorImp) ConsumeTraces(ctx context.Context, traces pdata.Traces) error {
	p.lock.Lock()

	p.aggregateMetrics(traces)

	m, err := p.buildMetrics()
	p.reset()
	p.lock.Unlock()

	if err != nil {
		return err
	}

	// Firstly, export metrics to avoid being impacted by downstream trace processor errors/latency.
	if err := p.metricsExporter.ConsumeMetrics(ctx, *m); err != nil {
		return err
	}

	// Forward trace data unmodified.
	return p.nextConsumer.ConsumeTraces(ctx, traces)
}

// buildMetrics collects the computed raw metrics data, builds the metrics object and
// writes the raw metrics data into the metrics object.
func (p *processorImp) buildMetrics() (*pdata.Metrics, error) {
	m := pdata.NewMetrics()
	rms := m.ResourceMetrics()
	for _, key := range p.resourceKeyToDimensions.Keys() {
		cachedResourceAttributesMap, ok := p.resourceKeyToDimensions.Get(key)
		if !ok {
			return nil, errors.New("expected cached resource attributes not found")
		}

		resourceAttributesMap, ok := cachedResourceAttributesMap.(pdata.AttributeMap)
		if !ok {
			return nil, errors.New("expected cached resource attributes type assertion failed")
		}

		// If the service name doesn't exist, we treat it as invalid and do not generate a metric
		if _, ok = resourceAttributesMap.Get(serviceNameKey); !ok {
			continue
		}

		rm := rms.AppendEmpty()

		// Iterate over `AttributeMap` structure defining resource attributes to append to the metric resource and append
		resourceAttributesMap.Range(func(k string, v pdata.AttributeValue) bool {
			rm.Resource().Attributes().Insert(k, v)
			return true
		})

		// build metrics per resource
		resourceAttrKey, ok := key.(resourceKey)
		if !ok {
			return nil, errors.New("resource key type assertion failed")
		}

		if err := p.collectCallMetrics(rm, resourceAttrKey); err != nil {
			return nil, err
		}

		if err := p.collectLatencyMetrics(rm, resourceAttrKey); err != nil {
			return nil, err
		}

	}
	return &m, nil
}

// collectLatencyMetrics collects the raw latency metrics, writing the data
// into the given instrumentation library metrics.
func (p *processorImp) collectLatencyMetrics(rm pdata.ResourceMetrics, resAttrKey resourceKey) error {
	for libKey := range p.latencyCount[resAttrKey] {
		ilm := rm.InstrumentationLibraryMetrics().AppendEmpty()
		ilm.InstrumentationLibrary().SetName(string(libKey))
		for mKey := range p.latencyCount[resAttrKey][libKey] {
			mLatency := ilm.Metrics().AppendEmpty()
			mLatency.SetDataType(pdata.MetricDataTypeHistogram)
			mLatency.SetName("latency")
			mLatency.Histogram().SetAggregationTemporality(p.config.GetAggregationTemporality())

			timestamp := pdata.TimestampFromTime(time.Now())

			dpLatency := mLatency.Histogram().DataPoints().AppendEmpty()
			dpLatency.SetStartTimestamp(pdata.TimestampFromTime(p.startTime))
			dpLatency.SetTimestamp(timestamp)
			dpLatency.SetExplicitBounds(p.latencyBounds)
			dpLatency.SetBucketCounts(p.latencyBucketCounts[resAttrKey][libKey][mKey])
			dpLatency.SetCount(p.latencyCount[resAttrKey][libKey][mKey])
			dpLatency.SetSum(p.latencySum[resAttrKey][libKey][mKey])

			setLatencyExemplars(p.latencyExemplarsData[resAttrKey][libKey][mKey], timestamp, dpLatency.Exemplars())

			dimensions, err := p.getDimensionsByMetricKey(mKey)
			if err != nil {
				p.logger.Error(err.Error())
				return err
			}

			dimensions.Range(func(k string, v pdata.AttributeValue) bool {
				dpLatency.LabelsMap().Upsert(k, tracetranslator.AttributeValueToString(v))
				return true
			})
		}
	}
	return nil
}

// collectCallMetrics collects the raw call count metrics, writing the data
// into the given instrumentation library metrics.
func (p *processorImp) collectCallMetrics(rm pdata.ResourceMetrics, resAttrKey resourceKey) error {
	for libKey := range p.callSum[resAttrKey] {
		ilm := rm.InstrumentationLibraryMetrics().AppendEmpty()
		ilm.InstrumentationLibrary().SetName(string(libKey))
		for mKey := range p.callSum[resAttrKey][libKey] {
			mCalls := ilm.Metrics().AppendEmpty()
			mCalls.SetDataType(pdata.MetricDataTypeIntSum)
			mCalls.SetName("calls_total")
			mCalls.IntSum().SetIsMonotonic(true)
			mCalls.IntSum().SetAggregationTemporality(p.config.GetAggregationTemporality())

			dpCalls := mCalls.IntSum().DataPoints().AppendEmpty()
			dpCalls.SetStartTimestamp(pdata.TimestampFromTime(p.startTime))
			dpCalls.SetTimestamp(pdata.TimestampFromTime(time.Now()))
			dpCalls.SetValue(p.callSum[resAttrKey][libKey][mKey])

			dimensions, err := p.getDimensionsByMetricKey(mKey)
			if err != nil {
				p.logger.Error(err.Error())
				return err
			}

			dimensions.Range(func(k string, v pdata.AttributeValue) bool {
				dpCalls.LabelsMap().Upsert(k, tracetranslator.AttributeValueToString(v))
				return true
			})
		}
	}
	return nil
}

// getDimensionsByMetricKey gets dimensions from `metricKeyToDimensions` cache.
func (p *processorImp) getDimensionsByMetricKey(k metricKey) (*pdata.AttributeMap, error) {
	if item, ok := p.metricKeyToDimensions.Get(k); ok {
		if attributeMap, ok := item.(pdata.AttributeMap); ok {
			return &attributeMap, nil
		}
		return nil, fmt.Errorf("type assertion of metricKeyToDimensions attributes failed, the key is %q", k)
	}

	return nil, fmt.Errorf("value not found in metricKeyToDimensions cache by key %q", k)
}

// aggregateMetrics aggregates the raw metrics from the input trace data.
// Each metric is identified by a key that is built from the service name
// and span metadata such as operation, kind, status_code and any additional
// dimensions and resource attributes the user has configured.
func (p *processorImp) aggregateMetrics(traces pdata.Traces) {
	for i := 0; i < traces.ResourceSpans().Len(); i++ {
		rspans := traces.ResourceSpans().At(i)
		r := rspans.Resource()

		attr, ok := r.Attributes().Get(conventions.AttributeServiceName)
		if !ok {
			continue
		}
		serviceName := attr.StringVal()
		p.aggregateMetricsForServiceSpans(rspans, serviceName)
	}
}

func (p *processorImp) aggregateMetricsForServiceSpans(rspans pdata.ResourceSpans, serviceName string) {
	ilsSlice := rspans.InstrumentationLibrarySpans()
	instrLibName := instrLibKey(instrumentationLibraryName)
	for j := 0; j < ilsSlice.Len(); j++ {
		ils := ilsSlice.At(j)

		// if config is set to inherit instrumentation library name, then assume from trace
		// otherwise use default
		if p.inheritInstrumentationLibraryName {
			instrLibName = instrLibKey(ils.InstrumentationLibrary().Name())
		}

		spans := ils.Spans()
		for k := 0; k < spans.Len(); k++ {
			span := spans.At(k)
			aggrMeta := aggregationMeta{
				serviceName:  serviceName,
				resourceAttr: rspans.Resource().Attributes(),
				instrLibName: instrLibName,
			}
			p.aggregateMetricsForSpan(span, aggrMeta)
		}
	}
}

func (p *processorImp) aggregateMetricsForSpan(span pdata.Span, aggregationMeta aggregationMeta) {
	latencyInMilliseconds := float64(span.EndTimestamp()-span.StartTimestamp()) / float64(time.Millisecond.Nanoseconds())

	// Binary search to find the latencyInMilliseconds bucket index.
	index := sort.SearchFloat64s(p.latencyBounds, latencyInMilliseconds)

	mKey := p.buildMetricKey(span, aggregationMeta.resourceAttr, p.attachSpanAndTraceID)
	resourceAttrKey := p.buildResourceAttrKey(aggregationMeta.serviceName, aggregationMeta.resourceAttr)

	p.cacheMetricKey(span, mKey, aggregationMeta.resourceAttr)
	p.cacheResourceAttrKey(aggregationMeta.serviceName, aggregationMeta.resourceAttr, resourceAttrKey)
	p.updateCallMetrics(resourceAttrKey, mKey, aggregationMeta.instrLibName)
	p.updateLatencyMetrics(resourceAttrKey, mKey, latencyInMilliseconds, index, aggregationMeta.instrLibName)
	p.updateLatencyExemplars(resourceAttrKey, mKey, latencyInMilliseconds, span.TraceID(), aggregationMeta.instrLibName)
}

// updateCallMetrics increments the call count for the given metric key.
func (p *processorImp) updateCallMetrics(rKey resourceKey, mKey metricKey, iKey instrLibKey) {
	if _, ok := p.callSum[rKey]; !ok {
		p.callSum[rKey] = make(map[instrLibKey]map[metricKey]int64)
	}

	if _, ok := p.callSum[rKey][iKey]; !ok {
		p.callSum[rKey][iKey] = make(map[metricKey]int64)
	}

	p.callSum[rKey][iKey][mKey]++
}

func (p *processorImp) reset() {
	// If delta metrics, reset accumulated data
	if p.config.GetAggregationTemporality() == pdata.AggregationTemporalityDelta {
		p.resetAccumulatedMetrics()
	} else {
		p.metricKeyToDimensions.RemoveEvictedItems()
		p.resourceKeyToDimensions.RemoveEvictedItems()
	}

	p.resetExemplarData()
}

// resetAccumulatedMetrics resets the internal maps used to store created metric data. Also purge the cache for
// metricKeyToDimensions.
func (p *processorImp) resetAccumulatedMetrics() {
	p.callSum = make(map[resourceKey]map[instrLibKey]map[metricKey]int64)
	p.latencyCount = make(map[resourceKey]map[instrLibKey]map[metricKey]uint64)
	p.latencySum = make(map[resourceKey]map[instrLibKey]map[metricKey]float64)
	p.latencyBucketCounts = make(map[resourceKey]map[instrLibKey]map[metricKey][]uint64)
	p.metricKeyToDimensions.Purge()
	p.resourceKeyToDimensions.Purge()
}

// updateLatencyExemplars sets the histogram exemplars for the given resource and metric key and append the exemplar data.
func (p *processorImp) updateLatencyExemplars(rKey resourceKey, mKey metricKey, value float64, traceID pdata.TraceID, instrLibName instrLibKey) {
	if _, ok := p.latencyExemplarsData[rKey]; !ok {
		p.latencyExemplarsData[rKey] = make(map[instrLibKey]map[metricKey][]exemplarData)
	}

	rled, ok := p.latencyExemplarsData[rKey][instrLibName]
	if !ok {
		rled = make(map[metricKey][]exemplarData)
		p.latencyExemplarsData[rKey][instrLibName] = rled
	}

	rled[mKey] = append(rled[mKey], exemplarData{
		traceID: traceID,
		value:   value,
	})
}

// resetExemplarData resets the entire exemplars map so the next trace will recreate all
// the data structure. An exemplar is a punctual value that exists at specific moment in time
// and should be not considered like a metrics that persist over time.
func (p *processorImp) resetExemplarData() {
	p.latencyExemplarsData = make(map[resourceKey]map[instrLibKey]map[metricKey][]exemplarData)
}

// updateLatencyMetrics increments the histogram counts for the given metric key and bucket index.
func (p *processorImp) updateLatencyMetrics(rKey resourceKey, mKey metricKey, latency float64, index int, instrLibName instrLibKey) {
	// update latency bucket counts
	if _, ok := p.latencyBucketCounts[rKey]; !ok {
		p.latencyBucketCounts[rKey] = make(map[instrLibKey]map[metricKey][]uint64)
	}
	if _, ok := p.latencyBucketCounts[rKey][instrLibName]; !ok {
		p.latencyBucketCounts[rKey][instrLibName] = make(map[metricKey][]uint64)
	}
	if _, ok := p.latencyBucketCounts[rKey][instrLibName][mKey]; !ok {
		p.latencyBucketCounts[rKey][instrLibName][mKey] = make([]uint64, len(p.latencyBounds))
	}
	p.latencyBucketCounts[rKey][instrLibName][mKey][index]++

	// update latency sum
	if _, ok := p.latencySum[rKey]; !ok {
		p.latencySum[rKey] = make(map[instrLibKey]map[metricKey]float64)
	}
	if _, ok := p.latencySum[rKey][instrLibName]; !ok {
		p.latencySum[rKey][instrLibName] = make(map[metricKey]float64)
	}
	p.latencySum[rKey][instrLibName][mKey] += latency

	// update latency count
	if _, ok := p.latencyCount[rKey]; !ok {
		p.latencyCount[rKey] = make(map[instrLibKey]map[metricKey]uint64)
	}
	if _, ok := p.latencyCount[rKey][instrLibName]; !ok {
		p.latencyCount[rKey][instrLibName] = make(map[metricKey]uint64)
	}
	p.latencyCount[rKey][instrLibName][mKey]++
}

func (p *processorImp) buildDimensionKVs(span pdata.Span, optionalDims []Dimension, resourceAttrs pdata.AttributeMap, attachSpanAndTraceID bool) pdata.AttributeMap {
	dims := pdata.NewAttributeMap()

	dims.UpsertString(operationKey, span.Name())
	dims.UpsertString(spanKindKey, span.Kind().String())
	dims.UpsertString(statusCodeKey, span.Status().Code().String())

	if attachSpanAndTraceID {
		dims.UpsertString(spanIDKey, span.SpanID().HexString())
		dims.UpsertString(traceIDKey, span.TraceID().HexString())
	}
	for _, d := range optionalDims {
		if v, ok := getDimensionValue(d, span.Attributes(), resourceAttrs); ok {
			dims.Upsert(d.Name, v)
		}
	}
	return dims
}

func extractResourceAttrsByKeys(serviceName string, keys []Dimension, resourceAttrs pdata.AttributeMap) pdata.AttributeMap {
	dims := pdata.NewAttributeMap()
	dims.UpsertString(serviceNameKey, serviceName)
	for _, ra := range keys {
		if attr, ok := resourceAttrs.Get(ra.Name); ok {
			dims.Upsert(ra.Name, attr)
		} else if ra.Default != nil {
			// Set the default if configured, otherwise this metric should have no value set for the resource attribute.
			dims.Upsert(ra.Name, pdata.NewAttributeValueString(*ra.Default))
		}
	}
	return dims
}

// buildMetricKey builds the metric key from the service name and span metadata such as operation, kind, status_code and
// will attempt to add any additional dimensions the user has configured that match the span's attributes
// or resource attributes. If the dimension exists in both, the span's attributes, being the most specific, takes precedence.
//
// The metric key is a simple concatenation of dimension values, delimited by a null character.
func (p *processorImp) buildMetricKey(span pdata.Span, resourceAttrs pdata.AttributeMap, attachSpanAndTraceID bool) metricKey {
	mkb := keybuilder.New()
	mkb.Append(
		span.Name(),
		span.Kind().String(),
		span.Status().Code().String(),
	)

	if attachSpanAndTraceID {
		mkb.Append(
			span.SpanID().HexString(),
			span.TraceID().HexString(),
		)
	}

	for _, d := range p.dimensions {
		if v, ok := getDimensionValue(d, span.Attributes(), resourceAttrs); ok {
			mkb.Append(tracetranslator.AttributeValueToString(v))
		}
	}

	k := metricKey(mkb.String())
	return k
}

// buildResourceAttrKey builds the metric key from the service name and will attempt to add any additional resource attributes
// the user has configured that match the span's attributes
//
// The resource attribute key is a simple concatenation of the service name and the other specified resource attribute
// values, delimited by a null character.
func (p *processorImp) buildResourceAttrKey(serviceName string, resourceAttr pdata.AttributeMap) resourceKey {
	rkb := keybuilder.New()
	rkb.Append(serviceName)

	for _, ra := range p.resourceAttributes {
		if attr, ok := resourceAttr.Get(ra.Name); ok {
			rkb.Append(tracetranslator.AttributeValueToString(attr))
		} else if ra.Default != nil {
			// Set the default if configured, otherwise this metric should have no value set for the resource attribute.
			rkb.Append(*ra.Default)
		}
	}

	k := resourceKey(rkb.String())
	return k
}

// getDimensionValue gets the dimension value for the given configured dimension.
// It searches through the span's attributes first, being the more specific;
// falling back to searching in resource attributes if it can't be found in the span.
// Finally, falls back to the configured default value if provided.
//
// The ok flag indicates if a dimension value was fetched in order to differentiate
// an empty string value from a state where no value was found.
// todo - consider this: Given we are building resource attributes for the metrics, does that make sense to fallback the
// search from resource attributes anymore?
func getDimensionValue(d Dimension, spanAttr pdata.AttributeMap, resourceAttr pdata.AttributeMap) (v pdata.AttributeValue, ok bool) {
	// The more specific span attribute should take precedence.
	if attr, exists := spanAttr.Get(d.Name); exists {
		return attr, true
	}
	if attr, exists := resourceAttr.Get(d.Name); exists {
		return attr, true
	}
	// Set the default if configured, otherwise this metric will have no value set for the dimension.
	if d.Default != nil {
		return pdata.NewAttributeValueString(*d.Default), true
	}
	return v, ok
}

// cache the dimension key-value map for the metricKey if there is a cache miss.
// This enables a lookup of the dimension key-value map when constructing the metric like so:
// LabelsMap().InitFromMap(p.metricKeyToDimensions[key])
func (p *processorImp) cacheMetricKey(span pdata.Span, k metricKey, resourceAttrs pdata.AttributeMap) {
	p.metricKeyToDimensions.ContainsOrAdd(k, p.buildDimensionKVs(span, p.dimensions, resourceAttrs, p.attachSpanAndTraceID))
}

// cache the dimension key-value map for the resourceAttrKey if there is a cache miss.
// This enables a lookup of the dimension key-value map when constructing the resource.
func (p *processorImp) cacheResourceAttrKey(serviceName string, resourceAttrs pdata.AttributeMap, k resourceKey) {
	p.resourceKeyToDimensions.ContainsOrAdd(k, extractResourceAttrsByKeys(serviceName, p.resourceAttributes, resourceAttrs))
}

// copied from prometheus-go-metric-exporter
// sanitize replaces non-alphanumeric characters with underscores in s.
func sanitize(s string) string {
	if len(s) == 0 {
		return s
	}

	// Note: No length limit for label keys because Prometheus doesn't
	// define a length limit, thus we should NOT be truncating label keys.
	// See https://github.com/orijtech/prometheus-go-metrics-exporter/issues/4.
	s = strings.Map(sanitizeRune, s)
	if unicode.IsDigit(rune(s[0])) {
		s = "key_" + s
	}
	if s[0] == '_' {
		s = "key" + s
	}
	return s
}

// copied from prometheus-go-metric-exporter
// sanitizeRune converts anything that is not a letter or digit to an underscore
func sanitizeRune(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return r
	}
	// Everything else turns into an underscore
	return '_'
}

// setLatencyExemplars sets the histogram exemplars.
func setLatencyExemplars(exemplarsData []exemplarData, timestamp pdata.Timestamp, exemplars pdata.ExemplarSlice) {
	es := pdata.NewExemplarSlice()

	for _, ed := range exemplarsData {
		value := ed.value
		traceID := ed.traceID

		exemplar := es.AppendEmpty()

		if traceID.IsEmpty() {
			continue
		}

		exemplar.SetValue(value)
		exemplar.SetTimestamp(timestamp)
		exemplar.FilteredLabels().Insert(traceIDKey, pdata.NewAttributeValueString(traceID.HexString()).StringVal())
	}

	es.CopyTo(exemplars)
}
