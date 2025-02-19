/*
 * Copyright (c) 2023 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package service

import (
	"context"
	"time"

	logging "github.com/op/go-logging"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"

	"github.com/deepflowio/deepflow/server/querier/app/prometheus/model"
	"github.com/deepflowio/deepflow/server/querier/common"
	"github.com/deepflowio/deepflow/server/querier/config"
)

var log = logging.MustGetLogger("promethues")

type PrometheusService struct {
	// keep only 1 instance of prometheus engine during server lifetime
	engine   *promql.Engine
	executor *prometheusExecutor
}

func NewPrometheusService() *PrometheusService {
	// query.max-samples set to same default value in prometheus, ref settings: https://github.com/prometheus/prometheus/blob/main/cmd/prometheus/main.go#L407
	return &PrometheusService{
		engine: promql.NewEngine(promql.EngineOpts{
			Logger:                   newPrometheusLogger(),
			Reg:                      nil,
			MaxSamples:               config.Cfg.Prometheus.MaxSamples,
			Timeout:                  100 * time.Second,
			NoStepSubqueryIntervalFn: func(int64) int64 { return durationMilliseconds(1 * time.Minute) },
			EnableAtModifier:         true,
			EnableNegativeOffset:     true,
			EnablePerStepStats:       true,
		}),
		executor: NewPrometheusExecutor(),
	}
}

func (s *PrometheusService) PromRemoteReadService(req *prompb.ReadRequest, ctx context.Context) (resp *prompb.ReadResponse, err error) {
	return s.executor.promRemoteReadExecute(ctx, req)
}

func (s *PrometheusService) PromInstantQueryService(args *model.PromQueryParams, ctx context.Context) (*model.PromQueryResponse, error) {
	return s.executor.promQueryExecute(ctx, args, s.engine)
}

func (s *PrometheusService) PromRangeQueryService(args *model.PromQueryParams, ctx context.Context) (*model.PromQueryResponse, error) {
	return s.executor.promQueryRangeExecute(ctx, args, s.engine)
}

func (s *PrometheusService) PromLabelValuesService(args *model.PromMetaParams, ctx context.Context) (*model.PromQueryResponse, error) {
	return s.executor.getTagValues(ctx, args)
}

func (s *PrometheusService) PromSeriesQueryService(args *model.PromQueryParams, ctx context.Context) (*model.PromQueryResponse, error) {
	return s.executor.series(ctx, args)
}

func (s *PrometheusService) PromQLAnalysis(ctx context.Context, metric string, targetLabels []string, appLabels []string, startTime string, endTime string) (*common.Result, error) {
	return s.executor.promQLAnalysis(ctx, metric, targetLabels, appLabels, startTime, endTime)
}

func durationMilliseconds(d time.Duration) int64 {
	return int64(d / (time.Millisecond / time.Nanosecond))
}
