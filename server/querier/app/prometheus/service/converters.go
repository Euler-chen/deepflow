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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/prometheus/prometheus/prompb"

	"github.com/deepflowio/deepflow/server/libs/datastructure"
	"github.com/deepflowio/deepflow/server/querier/common"
	"github.com/deepflowio/deepflow/server/querier/config"
	chCommon "github.com/deepflowio/deepflow/server/querier/engine/clickhouse/common"
	tagdescription "github.com/deepflowio/deepflow/server/querier/engine/clickhouse/tag"
	"github.com/deepflowio/deepflow/server/querier/engine/clickhouse/view"
)

const (
	PROMETHEUS_METRICS_NAME     = "__name__"
	EXT_METRICS_NATIVE_TAG_NAME = "tag"
	EXT_METRICS_TIME_COLUMNS    = "timestamp"
)

const (
	EXT_METRICS_TABLE         = "metrics"
	PROMETHEUS_TABLE          = "samples"
	L4_FLOW_LOG_TABLE         = "l4_flow_log"
	L7_FLOW_LOG_TABLE         = "l7_flow_log"
	VTAP_APP_PORT_TABLE       = "vtap_app_port"
	VTAP_FLOW_PORT_TABLE      = "vtap_flow_port"
	VTAP_APP_EDGE_PORT_TABLE  = "vtap_app_edge_port"
	VTAP_FLOW_EDGE_PORT_TABLE = "vtap_flow_edge_port"
)

const (
	// map tag will be extract in other tags
	// e.g.: tag `k8s.label` will extract in tag `k8s.label.app` (or other)
	IGNORABLE_TAG_TYPE = "map"
)

var QPSLeakyBucket *datastructure.LeakyBucket

// /querier/engine/clickhouse/clickhouse.go: `pod_ingress` and `lb_listener` are not supported by select
// `time` as tag is pointless
var ignorableTagNames = []string{"pod_ingress", "lb_listener", "time"}

var edgeTableNames = []string{
	VTAP_FLOW_EDGE_PORT_TABLE,
	VTAP_APP_EDGE_PORT_TABLE,
	L4_FLOW_LOG_TABLE,
	L7_FLOW_LOG_TABLE,
}

// rules for convert prom tag to native tag when query prometheus metrics
var matcherRules = map[string]string{
	"k8s_label_": "k8s.label.",
	"cloud_tag_": "cloud.tag.",
}

// definition: https://github.com/prometheus/prometheus/blob/main/promql/parser/lex.go#L106
// docs: https://prometheus.io/docs/prometheus/latest/querying/operators/#aggregation-operators
// convert promql aggregation functions to querier functions
var aggFunctions = map[string]string{
	"sum":          view.FUNCTION_SUM,
	"avg":          view.FUNCTION_AVG,
	"count":        view.FUNCTION_COUNT, // in querier should query Sum(log_count)
	"min":          view.FUNCTION_MIN,
	"max":          view.FUNCTION_MAX,
	"group":        "1", // all values in the resulting vector are 1
	"stddev":       view.FUNCTION_STDDEV,
	"stdvar":       "",                  // not supported
	"topk":         "",                  // not supported in querier, but clickhouse does, FIXME: should supported in querier
	"bottomk":      "",                  // not supported
	"count_values": view.FUNCTION_COUNT, // equals count() group by value in ck
	"quantile":     "",                  // not supported, FIXME: should support histogram in querier, and calcul Pxx by histogram
}

// define `showtag` flag, it passed when and only [api/v1/series] been called
type CtxKeyShowTag struct{}

type prefix int

const (
	prefixNone     prefix = iota
	prefixDeepFlow        // support "df_" prefix for DeepFlow universal tag, e.g.: df_auto_instance
	prefixTag             // support "tag_" prefix for Prometheus native lable, e.g.: tag_instance
)

type ctxKeyPrefixType struct{}

func promReaderTransToSQL(ctx context.Context, req *prompb.ReadRequest) (context.Context, string, string, string, error) {
	// QPS Limit Check
	// Both SetRate and Acquire are expanded by 1000 times, making it suitable for small QPS scenarios.
	if !QPSLeakyBucket.Acquire(1000) {
		return ctx, "", "", "", errors.New("Prometheus query rate exceeded!")
	}

	queriers := req.Queries
	if len(queriers) < 1 {
		return ctx, "", "", "", errors.New("len(req.Queries) == 0, this feature is not yet implemented!")
	}
	q := queriers[0]
	startTime := q.Hints.StartMs / 1000
	endTime := q.Hints.EndMs / 1000
	if q.EndTimestampMs%1000 > 0 {
		endTime += 1
	}

	prefixType, metricName, db, table, dataPrecision, metricAlias, err := parseMetric(q.Matchers)
	ctx = context.WithValue(ctx, ctxKeyPrefixType{}, prefixType)
	if err != nil {
		return ctx, "", "", "", err
	}

	metricsArray := []string{fmt.Sprintf("toUnixTimestamp(time) AS %s", EXT_METRICS_TIME_COLUMNS)}
	var groupBy []string
	var metricWithAggFunc string

	isShowTagStatement := false
	if st, ok := ctx.Value(CtxKeyShowTag{}).(bool); ok {
		isShowTagStatement = st
	}
	if isShowTagStatement {
		tagsArray, err := showTags(ctx, db, table, startTime, endTime)
		if err != nil {
			return ctx, "", "", "", err
		}
		// append all `SHOW tags`
		metricsArray = append(metricsArray, tagsArray...)
	} else {
		if db != DB_NAME_EXT_METRICS && db != DB_NAME_DEEPFLOW_SYSTEM && db != chCommon.DB_NAME_PROMETHEUS && db != "" {
			// DeepFlow native metrics needs aggregation for query
			if len(q.Hints.Grouping) == 0 {
				// not specific cardinality
				return ctx, "", "", "", fmt.Errorf("unknown series")
			}
			if !q.Hints.By {
				// not support for `without` operation
				return ctx, "", "", "", fmt.Errorf("not support for 'without' clause for aggregation")
			}

			aggOperator := aggFunctions[q.Hints.Func]
			if aggOperator == "" {
				return ctx, "", "", "", fmt.Errorf("aggregation operator: %s is not supported yet", q.Hints.Func)
			}

			// time query per step
			if q.Hints.StepMs > 0 {
				// range query, aggregation for time step
				// rebuild `time` in range query, replace `toUnixTimestamp(time) as timestamp`
				// time(time, x) means aggregation with time by interval x
				metricsArray[0] = fmt.Sprintf("time(time, %d) AS %s", q.Hints.StepMs/1e3, EXT_METRICS_TIME_COLUMNS)
			}

			groupBy = make([]string, 0, len(q.Hints.Grouping)+1)
			// instant query only aggerate to 1 timestamp point
			groupBy = append(groupBy, EXT_METRICS_TIME_COLUMNS)

			// should append all labels in query & grouping clause
			for _, groupLabel := range q.Hints.Grouping {
				label := fmt.Sprintf("`%s`", convertToQuerierAllowedTagName(groupLabel))
				groupBy = append(groupBy, label)
				metricsArray = append(metricsArray, label)
			}

			// aggregation for metrics, assert aggOperator is not empty
			switch aggOperator {
			case view.FUNCTION_SUM, view.FUNCTION_AVG, view.FUNCTION_MIN, view.FUNCTION_MAX, view.FUNCTION_STDDEV:
				metricWithAggFunc = fmt.Sprintf("%s(`%s`)", aggOperator, metricName)
			case "1":
				// group
				metricWithAggFunc = aggOperator
			case view.FUNCTION_COUNT:
				if db != chCommon.DB_NAME_FLOW_LOG {
					return ctx, "", "", "", fmt.Errorf("only supported Count for flow_log")
				}
				metricWithAggFunc = "Sum(`log_count`)" // will be append as `metrics.$metricsName` in below

				// count_values means count unique value
				if q.Hints.Func == "count_values" {
					metricsArray = append(metricsArray, fmt.Sprintf("`%s`", metricName)) // append original metric name
					groupBy = append(groupBy, fmt.Sprintf("`%s`", metricName))
				}
			}
		}
	}

	if db == "" || db == chCommon.DB_NAME_EXT_METRICS || db == chCommon.DB_NAME_DEEPFLOW_SYSTEM || db == chCommon.DB_NAME_PROMETHEUS {
		// append metricName as "`metrics.%s`"
		metricsArray = append(metricsArray, fmt.Sprintf(metricAlias, metricName))
		// append `tag` only for prometheus & ext_metrics & deepflow_system
		metricsArray = append(metricsArray, fmt.Sprintf("`%s`", EXT_METRICS_NATIVE_TAG_NAME))
	} else {
		// append metricName as "%s as `metrics.%s`"
		if metricWithAggFunc != "" {
			// only when query metric samples
			metricsArray = append(metricsArray, fmt.Sprintf(metricAlias, metricWithAggFunc, metricName))
		} else {
			// only when query series
			metricsArray = append(metricsArray, fmt.Sprintf(metricAlias, metricName, metricName))
		}
	}

	filters := make([]string, 0, len(q.Matchers)+1)
	filters = append(filters, fmt.Sprintf("(time >= %d AND time <= %d)", startTime, endTime))
	for _, matcher := range q.Matchers {
		if matcher.Name == PROMETHEUS_METRICS_NAME {
			continue
		}
		operation := getLabelMatcherType(matcher.Type)
		if operation == "" {
			return ctx, "", "", "", fmt.Errorf("unknown match type %v", matcher.Type)
		}

		switch db {
		case "", chCommon.DB_NAME_DEEPFLOW_SYSTEM:
			if strings.HasPrefix(matcher.Name, config.Cfg.Prometheus.AutoTaggingPrefix) {
				tagName := convertToQuerierAllowedTagName(removeDeepFlowPrefix(matcher.Name))
				filters = append(filters, fmt.Sprintf("`%s` %s '%s'", tagName, operation, matcher.Value))

				// when PromQL mention a deepflow universal tag, append into metrics
				if config.Cfg.Prometheus.RequestQueryWithDebug {
					metricsArray = append(metricsArray, tagName)
				}
			} else {
				filters = append(filters, fmt.Sprintf("`tag.%s` %s '%s'", matcher.Name, operation, matcher.Value))
				// append in query for analysis (findout if tag is target_label)
				metricsArray = append(metricsArray, fmt.Sprintf("`tag.%s`", matcher.Name))
			}
		default:
			// deepflow metrics (vtap_app/flow_part/edge_part & ext_metrics & prometheus)
			if strings.HasPrefix(matcher.Name, "tag_") {
				tagName := removeTagPrefix(matcher.Name)
				filters = append(filters, fmt.Sprintf("`tag.%s` %s '%s'", tagName, operation, matcher.Value))
				// for prometheus native tag, append in query for analysis (findout if tag is target_label)
				if config.Cfg.Prometheus.RequestQueryWithDebug {
					metricsArray = append(metricsArray, fmt.Sprintf("`tag.%s`", tagName))
				}
			} else {
				// convert k8s label tag to query tag
				tagName := convertToQuerierAllowedTagName(matcher.Name)
				filters = append(filters, fmt.Sprintf("`%s` %s '%s'", tagName, operation, matcher.Value))
			}
		}
	}

	sql := parseToQuerierSQL(ctx, db, table, metricsArray, filters, groupBy)
	return ctx, sql, db, dataPrecision, err
}

func parseMetric(matchers []*prompb.LabelMatcher) (prefixType prefix, metricName string, db string, table string, dataPrecision string, metricAlias string, err error) {
	// get metric_name from the matchers
	for _, matcher := range matchers {
		if matcher.Name != PROMETHEUS_METRICS_NAME {
			continue
		}
		metricName = matcher.Value

		if strings.Contains(metricName, "__") {
			// DeepFlow native metrics: ${db}__${table}__${metricsName}
			// i.e.: flow_log__l4_flow_log__byte_rx
			// DeepFlow native metrics(flow_metrics): ${db}__${table}__${metricsName}__${datasource}
			// i.e.: flow_metrics__vtap_flow_port__byte_rx__1m
			// Prometheus/InfluxDB integrated metrics: ext_metrics__metrics__${integratedSource}_${metricsName}
			// i.e.: ext_metrics__metrics__prometheus_node_cpu_seconds_total
			// Prometheus integrated metrics: prometheus__samples__${metricsName}
			// i.e.: prometheus__samples__node_cpu_seconds_total
			metricsSplit := strings.Split(metricName, "__")
			if _, ok := chCommon.DB_TABLE_MAP[metricsSplit[0]]; ok {
				db = metricsSplit[0]
				table = metricsSplit[1] // FIXME: should fix deepflow_system table name like 'deepflow_server.xxx'
				metricName = metricsSplit[2]

				if db == DB_NAME_DEEPFLOW_SYSTEM {
					metricAlias = "metrics.%s"
				} else if db == DB_NAME_EXT_METRICS {
					// identify tag prefix as "tag_"
					prefixType = prefixTag
					// convert prometheus_xx/influxdb_xx to prometheus.xxx/influxdb.xx (split to 2 parts)
					realMetrics := strings.SplitN(metricName, "_", 2)
					if len(realMetrics) > 1 {
						table = fmt.Sprintf("%s.%s", realMetrics[0], realMetrics[1])
						metricName = realMetrics[1]
					}
					metricAlias = "metrics.%s"
				} else if db == chCommon.DB_NAME_PROMETHEUS {
					prefixType = prefixTag
					// query `prometheus`.`samples` table, should query metrics
					table = metricName
					metricAlias = "value as `metrics.%s`"
				} else {
					// To identify which columns belong to metrics, we prefix all metrics names with `metrics.`
					metricAlias = "%s as `metrics.%s`"
				}

				// data precision only available for 'flow_metrics'
				if len(metricsSplit) > 3 {
					dataPrecision = metricsSplit[3]
				}
			} else {
				return prefixType, "", "", "", "", "", fmt.Errorf("unknown metrics %v", metricName)
			}
		} else {
			// Prometheus native metrics: ${metricsName}
			// identify prefix for tag names with "df_"
			prefixType = prefixDeepFlow

			// Prometheus native metrics only query `value` as metrics sample
			metricAlias = "value as `metrics.%s`"
			table = metricName
		}
		break
	}
	return
}

func showTags(ctx context.Context, db string, table string, startTime int64, endTime int64) ([]string, error) {
	showTags := "SHOW tags FROM %s.%s WHERE time >= %d AND time <= %d"
	var data *common.Result
	var err error
	var tagsArray []string
	if db == "" || db == chCommon.DB_NAME_PROMETHEUS {
		data, err = tagdescription.GetTagDescriptions(chCommon.DB_NAME_PROMETHEUS, PROMETHEUS_TABLE, fmt.Sprintf(showTags, chCommon.DB_NAME_PROMETHEUS, PROMETHEUS_TABLE, startTime, endTime), ctx)
	} else if db == chCommon.DB_NAME_EXT_METRICS {
		data, err = tagdescription.GetTagDescriptions(chCommon.DB_NAME_EXT_METRICS, EXT_METRICS_TABLE, fmt.Sprintf(showTags, chCommon.DB_NAME_EXT_METRICS, EXT_METRICS_TABLE, startTime, endTime), ctx)
	} else {
		data, err = tagdescription.GetTagDescriptions(db, table, fmt.Sprintf(showTags, db, table, startTime, endTime), ctx)
	}
	if err != nil {
		return tagsArray, err
	}

	if common.IsValueInSliceString(table, edgeTableNames) {
		tagsArray = make([]string, 0, len(data.Values)*2)
	} else {
		tagsArray = make([]string, 0, len(data.Values))
	}

	for _, value := range data.Values {
		// data.Columns definitions:
		// "columns": ["name","client_name","server_name","display_name","type","category","operators","permissions","description","related_tag"]
		// i.e.: columns[i] defines name of values[i]
		values := value.([]interface{})
		tagName := values[0].(string)
		if common.IsValueInSliceString(tagName, ignorableTagNames) {
			continue
		}
		clientTagName := values[1].(string)
		serverTagName := values[2].(string)
		tagType := values[4].(string)

		if tagType == IGNORABLE_TAG_TYPE {
			continue
		}

		// `edgeTable` storage data which contains both client and server-side, so metrics should cover both, else only one of them
		// e.g.: auto_instance_0/auto_instance_1 in `vtap_app_edge_port`, auto_instance in `vtap_app_port`
		if common.IsValueInSliceString(table, edgeTableNames) && tagName != clientTagName {
			tagsArray = append(tagsArray, fmt.Sprintf("`%s`", clientTagName))
			tagsArray = append(tagsArray, fmt.Sprintf("`%s`", serverTagName))
		} else {
			tagsArray = append(tagsArray, fmt.Sprintf("`%s`", tagName))
		}
	}

	return tagsArray, nil
}

func parseToQuerierSQL(ctx context.Context, db string, table string, metrics []string, filters []string, groupBy []string) (sql string) {
	// order by DESC for get data completely, then scan data reversely for data combine(see func.RespTransToProm)
	// querier will be called later, so there is no need to display the declaration db
	if db != "" {
		// FIXME: if db is ext_metrics, only support for prometheus metrics now
		sqlBuilder := strings.Builder{}
		sqlBuilder.WriteString(fmt.Sprintf("SELECT %s FROM %s WHERE %s ", strings.Join(metrics, ","), table, strings.Join(filters, " AND ")))
		if len(groupBy) > 0 {
			sqlBuilder.WriteString("GROUP BY " + strings.Join(groupBy, ","))
		}
		sqlBuilder.WriteString(fmt.Sprintf(" ORDER BY %s desc LIMIT %s", EXT_METRICS_TIME_COLUMNS, config.Cfg.Limit))
		sql = sqlBuilder.String()
	} else {
		sql = fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s desc LIMIT %s", strings.Join(metrics, ","),
			table, // equals prometheus metric name
			strings.Join(filters, " AND "),
			EXT_METRICS_TIME_COLUMNS, config.Cfg.Limit)
	}
	return
}

// querier result trans to Prom Response
func respTransToProm(ctx context.Context, result *common.Result) (resp *prompb.ReadResponse, err error) {
	tagIndex := -1
	metricsIndex := -1
	timeIndex := -1
	otherTagCount := 0
	metricsName := ""
	tagsFieldIndex := make(map[int]bool, len(result.Columns))
	prefix, _ := ctx.Value(ctxKeyPrefixType{}).(prefix) // ignore if key not exist
	for i, tag := range result.Columns {
		if tag == EXT_METRICS_NATIVE_TAG_NAME {
			tagIndex = i
		} else if strings.HasPrefix(tag.(string), "tag.") {
			tagsFieldIndex[i] = true
		} else if strings.HasPrefix(tag.(string), "metrics.") {
			metricsIndex = i
			metricsName = strings.TrimPrefix(tag.(string), "metrics.")
		} else if tag == EXT_METRICS_TIME_COLUMNS {
			timeIndex = i
		} else {
			otherTagCount++
		}
	}
	if metricsIndex < 0 || timeIndex < 0 {
		return nil, fmt.Errorf("metricsIndex(%d), timeIndex(%d) get failed", metricsIndex, timeIndex)
	}
	metricsType := result.Schemas[metricsIndex].ValueType

	// append other deepflow native tag into results
	allDeepFlowNativeTags := make([]int, 0, otherTagCount)
	for i := range result.Columns {
		if i == tagIndex || i == metricsIndex || i == timeIndex || tagsFieldIndex[i] {
			continue
		}
		allDeepFlowNativeTags = append(allDeepFlowNativeTags, i)
	}

	// Scan all the results, determine the seriesID of each sample and the number of samples in each series,
	// so that the size of the sample array in each series can be determined in advance.
	maxPossibleSeries := len(result.Values)
	if maxPossibleSeries > config.Cfg.Prometheus.SeriesLimit {
		maxPossibleSeries = config.Cfg.Prometheus.SeriesLimit
	}

	seriesIndexMap := map[string]int32{}                            // the index in seriesArray, for each `tagsJsonStr`
	seriesArray := make([]*prompb.TimeSeries, 0, maxPossibleSeries) // series storage
	sampleSeriesIndex := make([]int32, len(result.Values))          // the index in seriesArray, for each sample
	seriesSampleCount := make([]int32, maxPossibleSeries)           // number of samples of each series
	initialSeriesIndex := int32(0)

	tagsStrList := make([]string, 0, len(allDeepFlowNativeTags))
	for i, v := range result.Values {
		values := v.([]interface{})

		// merge and serialize all tags as map key
		var deepflowNativeTagString, promTagJson string
		// merge prometheus tags
		if tagIndex > -1 {
			promTagJson = values[tagIndex].(string)
			deepflowNativeTagString = promTagJson
		}

		// merge deepflow autotagging tags
		if len(allDeepFlowNativeTags) > 0 {
			for _, idx := range allDeepFlowNativeTags {
				tagsStrList = append(tagsStrList, strconv.Itoa(idx))
				tagsStrList = append(tagsStrList, getValue(values[idx]))
			}
			deepflowNativeTagString += strings.Join(tagsStrList, "-")
			tagsStrList = tagsStrList[:0]
		}

		// check and assign seriesIndex
		var series *prompb.TimeSeries
		index, exist := seriesIndexMap[deepflowNativeTagString]
		if exist {
			sampleSeriesIndex[i] = index
			seriesSampleCount[index]++
		} else {
			if len(seriesIndexMap) >= config.Cfg.Prometheus.SeriesLimit {
				sampleSeriesIndex[i] = -1
				continue
			}

			// tag label pair
			var pairs []prompb.Label
			if tagIndex > -1 {
				tagMap := make(map[string]string)
				json.Unmarshal([]byte(promTagJson), &tagMap)
				pairs = make([]prompb.Label, 0, 1+len(tagMap)+len(allDeepFlowNativeTags))
				if prefix == prefixTag {
					// prometheus tag for deepflow metrics
					for k, v := range tagMap {
						pairs = append(pairs, prompb.Label{
							Name:  appendPrometheusPrefix(k),
							Value: v,
						})
					}
				} else {
					// no prefix, use prometheus native tag
					for k, v := range tagMap {
						pairs = append(pairs, prompb.Label{
							Name:  k,
							Value: v,
						})
					}
				}

			}

			if cap(pairs) == 0 {
				pairs = make([]prompb.Label, 0, 1+len(allDeepFlowNativeTags))
			}

			for _, idx := range allDeepFlowNativeTags {
				// remove zero value tag (0/""/{})
				if isZero(values[idx]) {
					continue
				}
				if tagIndex > -1 && prefix == prefixDeepFlow {
					// deepflow tag for prometheus metrics
					pairs = append(pairs, prompb.Label{
						Name:  appendDeepFlowPrefix(formatTagName(result.Columns[idx].(string))),
						Value: getValue(values[idx]),
					})
				} else {
					pairs = append(pairs, prompb.Label{
						Name:  formatTagName(result.Columns[idx].(string)),
						Value: getValue(values[idx]),
					})
				}
			}

			// append the special tag: "__name__": "$metricsName"
			pairs = append(pairs, prompb.Label{
				Name:  PROMETHEUS_METRICS_NAME,
				Value: metricsName,
			})
			series = &prompb.TimeSeries{Labels: pairs}
			seriesArray = append(seriesArray, series)

			seriesIndexMap[deepflowNativeTagString] = initialSeriesIndex
			sampleSeriesIndex[i] = initialSeriesIndex
			seriesSampleCount[initialSeriesIndex] = 1
			initialSeriesIndex++
		}
	}

	// reverse scan, make data order by time asc for prometheus filter handling (happens in prometheus PromQL engine)
	for i := len(result.Values) - 1; i >= 0; i-- {
		if sampleSeriesIndex[i] == -1 {
			continue // SLIMIT overflow
		}

		// get metrics
		values := result.Values[i].([]interface{})
		var metricsValue float64
		if values[metricsIndex] == nil {
			metricsValue = 0
			continue
		}
		if metricsType == "Int" {
			metricsValue = float64(values[metricsIndex].(int))
		} else if metricsType == "Float64" {
			metricsValue = values[metricsIndex].(float64)
		} else {
			return nil, fmt.Errorf("Unknown metrics type %s, value = %v", metricsType, values[metricsIndex])
		}

		// add a sample for the TimeSeries
		seriesIndex := sampleSeriesIndex[i]
		series := seriesArray[seriesIndex]
		if cap(series.Samples) == 0 {
			series.Samples = make([]prompb.Sample, 0, seriesSampleCount[seriesIndex])
		}
		series.Samples = append(
			series.Samples, prompb.Sample{
				Timestamp: int64(values[timeIndex].(int)) * 1000,
				Value:     metricsValue,
			},
		)
	}

	// assemble the final prometheus response
	resp = &prompb.ReadResponse{
		Results: []*prompb.QueryResult{{}},
	}
	resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, seriesArray...)
	return resp, nil
}

// match prometheus lable matcher type
func getLabelMatcherType(t prompb.LabelMatcher_Type) string {
	switch t {
	case prompb.LabelMatcher_EQ:
		return "="
	case prompb.LabelMatcher_NEQ:
		return "!="
	case prompb.LabelMatcher_RE:
		return "REGEXP"
	case prompb.LabelMatcher_NRE:
		return "NOT REGEXP"
	default:
		return ""
	}
}

func getValue(value interface{}) string {
	switch val := value.(type) {
	case int:
		return strconv.Itoa(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case time.Time:
		return val.String()
	case nil:
		return ""
	default:
		return val.(string)
	}
}

func isZero(value interface{}) bool {
	switch val := value.(type) {
	case string:
		return val == "" || val == "{}"
	case int:
		return val == 0
	case nil:
		return true
	default:
		return false
	}
}

func formatTagName(tagName string) (newTagName string) {
	newTagName = strings.ReplaceAll(tagName, ".", "_")
	newTagName = strings.ReplaceAll(newTagName, "-", "_")
	newTagName = strings.ReplaceAll(newTagName, "/", "_")
	return newTagName
}

func appendDeepFlowPrefix(tag string) string {
	return fmt.Sprintf("%s%s", config.Cfg.Prometheus.AutoTaggingPrefix, tag)
}

func appendPrometheusPrefix(tag string) string {
	return fmt.Sprintf("tag_%s", tag)
}

// FIXME: should reverse `formatTagName` funtion, build a `tag` map during series query
func convertToQuerierAllowedTagName(matcherName string) (tagName string) {
	tagName = matcherName
	for k, v := range matcherRules {
		if strings.HasPrefix(tagName, k) {
			tagName = strings.Replace(tagName, k, v, 1)
			return tagName
		}
	}
	return tagName
}

func removeDeepFlowPrefix(tag string) string {
	return strings.TrimPrefix(tag, config.Cfg.Prometheus.AutoTaggingPrefix)
}

func removeTagPrefix(tag string) string {
	return strings.Replace(tag, "tag_", "", 1)
}
