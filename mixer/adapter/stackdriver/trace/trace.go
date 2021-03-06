// Copyright 2018 the Istio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package trace contains a tracespan adapter for Stackdriver trace.
package trace

import (
	"context"
	"fmt"

	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/plugin/ochttp/propagation/b3"
	"go.opencensus.io/trace"

	"istio.io/istio/mixer/adapter/stackdriver/config"
	"istio.io/istio/mixer/adapter/stackdriver/helper"
	"istio.io/istio/mixer/pkg/adapter"
	"istio.io/istio/mixer/template/tracespan"
)

type (
	builder struct {
		types map[string]*tracespan.Type
		mg    *helper.MetadataGenerator
		cfg   *config.Params
	}

	handler struct {
		te      trace.Exporter
		sampler trace.Sampler
	}
)

var (
	// compile-time assertion that we implement the interfaces we promise
	_ tracespan.HandlerBuilder = &builder{}
	_ tracespan.Handler        = &handler{}
)

const attrHTTPStatusCode = "http.status_code"

// NewBuilder returns a builder implementing the tracespan.HandlerBuilder interface.
func NewBuilder(mg *helper.MetadataGenerator) tracespan.HandlerBuilder {
	return &builder{mg: mg}
}

func (b *builder) SetTraceSpanTypes(types map[string]*tracespan.Type) {
	b.types = types
}

func (b *builder) SetAdapterConfig(cfg adapter.Config) {
	b.cfg = cfg.(*config.Params)
}

func (b *builder) Validate() (ce *adapter.ConfigErrors) {
	if t := b.cfg.Trace; t != nil {
		if t.SampleProbability < 0 || t.SampleProbability > 1 {
			ce = ce.Appendf("trace.sampleProbability", "sampling probability must be between 0 and 1 (inclusive)")
		}
	}
	return ce
}

func (b *builder) Build(ctx context.Context, env adapter.Env) (adapter.Handler, error) {
	exporter, err := getExporterFunc(ctx, env, b.cfg)
	if err != nil {
		return nil, err
	}

	h := &handler{
		te: exporter,
	}
	traceCfg := b.cfg.Trace
	if sampleProbability := traceCfg.SampleProbability; sampleProbability > 0 {
		h.sampler = trace.ProbabilitySampler(traceCfg.SampleProbability)
	}
	return h, nil
}

func (h *handler) HandleTraceSpan(_ context.Context, values []*tracespan.Instance) (retErr error) {
	if h.sampler == nil {
		// Tracing is not configured.
		return nil
	}

	numExported := 0
	for _, val := range values {
		parentContext, ok := extractParentContext(val)
		if !ok {
			continue
		}
		spanContext, ok := extractSpanContext(val, parentContext)
		if !ok {
			continue
		}

		decision := h.sampler(trace.SamplingParameters{
			ParentContext:   parentContext,
			TraceID:         spanContext.TraceID,
			SpanID:          spanContext.SpanID,
			Name:            val.SpanName,
			HasRemoteParent: true,
		})

		if !decision.Sample {
			continue
		}
		spanContext.TraceOptions = trace.TraceOptions(1 /*sampled*/)

		span := buildSpanData(val, parentContext, spanContext)
		h.te.ExportSpan(span)
		numExported++
	}

	if numExported > 0 {
		h.tryFlush()
	}

	return
}

func extractParentContext(val *tracespan.Instance) (trace.SpanContext, bool) {
	var (
		parentContext trace.SpanContext
		ok            bool
	)
	if parentContext.TraceID, ok = b3.ParseTraceID(val.TraceId); !ok {
		return trace.SpanContext{}, false
	}
	parentContext.SpanID, _ = b3.ParseSpanID(val.ParentSpanId)
	return parentContext, true
}

func extractSpanContext(val *tracespan.Instance, parent trace.SpanContext) (trace.SpanContext, bool) {
	var (
		spanContext trace.SpanContext
		ok          bool
	)
	spanContext.TraceID = parent.TraceID
	if spanContext.SpanID, ok = b3.ParseSpanID(val.SpanId); !ok {
		return trace.SpanContext{}, false
	}
	return spanContext, true
}

func buildSpanData(val *tracespan.Instance, parentContext trace.SpanContext, spanContext trace.SpanContext) *trace.SpanData {
	attributes := make(map[string]interface{})
	for k, v := range val.SpanTags {
		switch x := v.(type) {
		case string, int64, float64:
			attributes[k] = x
		default:
			attributes[k] = fmt.Sprintf("%v", x)
		}
	}

	var status trace.Status
	if val.HttpStatusCode > 0 {
		if _, ok := attributes[attrHTTPStatusCode]; !ok {
			attributes[attrHTTPStatusCode] = val.HttpStatusCode
		}
		status = ochttp.TraceStatus(int(val.HttpStatusCode), "")
	}

	return &trace.SpanData{
		SpanKind:     trace.SpanKindServer,
		Name:         val.SpanName,
		StartTime:    val.StartTime,
		EndTime:      val.EndTime,
		ParentSpanID: parentContext.SpanID,
		SpanContext: trace.SpanContext{
			TraceOptions: spanContext.TraceOptions,
			TraceID:      spanContext.TraceID,
			SpanID:       spanContext.SpanID,
		},
		HasRemoteParent: true,
		Status:          status,
		Attributes:      attributes,
	}
}

func (h *handler) Close() error {
	return nil
}

func (h *handler) tryFlush() {
	if flusher, ok := h.te.(flusher); ok {
		flusher.Flush()
	}
}

type flusher interface {
	Flush()
}
