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

package dbwriter

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	logging "github.com/op/go-logging"

	"github.com/deepflowio/deepflow/server/ingester/common"
	baseconfig "github.com/deepflowio/deepflow/server/ingester/config"
	"github.com/deepflowio/deepflow/server/ingester/flow_tag"
	"github.com/deepflowio/deepflow/server/ingester/pkg/ckwriter"
	"github.com/deepflowio/deepflow/server/ingester/prometheus/config"
	"github.com/deepflowio/deepflow/server/libs/ckdb"
	"github.com/deepflowio/deepflow/server/libs/datatype/prompb"
	"github.com/deepflowio/deepflow/server/libs/stats"
	"github.com/deepflowio/deepflow/server/libs/utils"
)

var log = logging.MustGetLogger("prometheus.dbwriter")

const (
	QUEUE_BATCH_SIZE = 1024
	PROMETHEUS_DB    = "prometheus"
	PROMETHEUS_TABLE = "samples"
)

type ClusterNode struct {
	Addr string
	Port uint16
}

type Counter struct {
	MetricsCount int64 `statsd:"metrics-count"`
	WriteErr     int64 `statsd:"write-err"`
}

// all 'PrometheusWriters' share 'prometheusCKWriters' to write to ClickHouse, preventing each PrometheusWriter from creating CKWriter and causing excessive resource consumption
var prometheusCKWriters [MAX_APP_LABEL_COLUMN_INDEX]*ckwriter.CKWriter
var prometheusCKWritersMutex sync.Mutex

func getPrometheusCKWriters(columnCount int) *ckwriter.CKWriter {
	return prometheusCKWriters[columnCount]
}

func setPrometheusCKWriters(columnCount int, w *ckwriter.CKWriter) {
	prometheusCKWriters[columnCount] = w
}

func lockPrometheusCKWriters() {
	prometheusCKWritersMutex.Lock()
}

func unlockPrometheusCKWriters() {
	prometheusCKWritersMutex.Unlock()
}

type PrometheusWriter struct {
	decoderIndex      int
	name              string
	ckdbAddrs         []string
	ckdbUsername      string
	ckdbPassword      string
	ckdbCluster       string
	ckdbStoragePolicy string
	ckdbColdStorages  map[string]*ckdb.ColdStorage
	ckdbTimeZone      string
	ttl               int
	writerConfig      baseconfig.CKWriterConfig
	ckdbWatcher       *baseconfig.Watcher

	ckdbConn common.DBs

	appLabelColumnIncrement int
	metricsWriterCache      *ckwriter.CKWriter // the writer for prometheus.metrics table
	flowTagWriter           *flow_tag.FlowTagWriter

	counter *Counter
	utils.Closable
}

func (w *PrometheusWriter) InitTable() error {
	if w.ckdbConn == nil {
		conn, err := common.NewCKConnections(w.ckdbAddrs, w.ckdbUsername, w.ckdbPassword)
		if err != nil {
			return err
		}
		w.ckdbConn = conn
	}
	_, err := w.ckdbConn.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", PROMETHEUS_DB))

	w.getOrCreateCkwriter(&PrometheusSample{AppLabelValueIDs: make([]uint32, 1)})
	return err
}

func (w *PrometheusWriter) getOrCreateCkwriter(s *PrometheusSample) (*ckwriter.CKWriter, error) {
	// AppLabelValueIDs[0] is target label
	if len(s.AppLabelValueIDs) == 0 {
		return nil, fmt.Errorf("AppLabelValueIDs is empty")
	}
	appLabelCount := len(s.AppLabelValueIDs) - 1
	if appLabelCount > MAX_APP_LABEL_COLUMN_INDEX {
		return nil, fmt.Errorf("the length of AppLabelValueIDs(%d) is > MAX_APP_LABEL_COLUMN_INDEX(%d)", len(s.AppLabelValueIDs), MAX_APP_LABEL_COLUMN_INDEX)
	}
	if writer := getPrometheusCKWriters(appLabelCount); writer != nil {
		return writer, nil
	}
	lockPrometheusCKWriters()
	defer unlockPrometheusCKWriters()
	// check again
	if writer := getPrometheusCKWriters(appLabelCount); writer != nil {
		return writer, nil
	}

	if w.ckdbConn == nil {
		conn, err := common.NewCKConnections(w.ckdbAddrs, w.ckdbUsername, w.ckdbPassword)
		if err != nil {
			return nil, err
		}
		w.ckdbConn = conn
	}

	startTime := time.Now()
	log.Infof("start create new ckwriter for prometheus, app label count: %d", appLabelCount)
	// 将要创建的表信息
	table := s.GenCKTable(w.ckdbCluster, w.ckdbStoragePolicy, w.ttl, ckdb.GetColdStorage(w.ckdbColdStorages, s.DatabaseName(), s.TableName()), appLabelCount)

	ckwriter, err := ckwriter.NewCKWriter(
		w.ckdbAddrs, w.ckdbUsername, w.ckdbPassword,
		fmt.Sprintf("%s-%s-%d-%d", w.name, s.TableName(), w.decoderIndex, appLabelCount), w.ckdbTimeZone,
		table, w.writerConfig.QueueCount, w.writerConfig.QueueSize, w.writerConfig.BatchSize, w.writerConfig.FlushTimeout)
	if err != nil {
		return nil, err
	}
	currentCount, err := w.getCurrentAppLabelColumnCount()
	if err != nil {
		return nil, err
	}

	if currentCount < appLabelCount {
		startIndex, endIndex := currentCount+1, appLabelCount
		if err := w.addAppLabelColumns(w.ckdbConn, startIndex, endIndex); err != nil {
			return nil, err
		}

		// 需要在cluseter其他节点也增加列
		if err := w.createTableOnCluster(table); err != nil {
			log.Warningf("other node failed when create table: %s", err)
		}
		if err := w.addAppLabelColumnsOnCluster(startIndex, endIndex); err != nil {
			log.Warningf("other node failed when add app_value_id columns which index from %d to %d: %s", startIndex, endIndex, err)
		}
	}

	ckwriter.Run()
	setPrometheusCKWriters(appLabelCount, ckwriter)
	log.Infof("finish create new ckwriter for prometheus, app label count: %d, cost time: %s", appLabelCount, time.Since(startTime))

	return ckwriter, nil
}

func (w *PrometheusWriter) createTableOnCluster(table *ckdb.Table) error {
	endpoints, err := w.ckdbWatcher.GetClickhouseEndpointsWithoutMyself()
	if err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		err := ckwriter.InitTable(fmt.Sprintf("%s:%d", endpoint.Host, endpoint.Port), w.ckdbUsername, w.ckdbPassword, w.ckdbTimeZone, table)
		if err != nil {
			log.Warningf("node %s:%d init table failed. err: %s", endpoint.Host, endpoint.Port, err)
		} else {
			log.Infof("node %s:%d init table %s success", endpoint.Host, endpoint.Port, table.LocalName)
		}
	}
	return nil
}

func (w *PrometheusWriter) addAppLabelColumnsOnCluster(startIndex, endIndex int) error {
	endpoints, err := w.ckdbWatcher.GetClickhouseEndpointsWithoutMyself()
	if err != nil {
		return err
	}

	ckdbAddrs := []string{}
	for _, endpoint := range endpoints {
		ckdbAddrs = append(ckdbAddrs, fmt.Sprintf("%s:%d", endpoint.Host, endpoint.Port))

	}
	conn, err := common.NewCKConnections(ckdbAddrs, w.ckdbUsername, w.ckdbPassword)
	if err != nil {
		return err
	}
	defer conn.Close()
	return w.addAppLabelColumns(conn, startIndex, endIndex)
}

func (w *PrometheusWriter) addAppLabelColumns(conn common.DBs, startIndex, endIndex int) error {
	for i := startIndex; i <= endIndex; i++ {
		for _, table := range []string{PROMETHEUS_TABLE + "_local", PROMETHEUS_TABLE} {
			_, err := conn.Exec(fmt.Sprintf("ALTER TABLE %s.`%s` ADD COLUMN app_label_value_id_%d %s",
				PROMETHEUS_DB, table, i, ckdb.UInt32))
			if err != nil {
				if strings.Contains(err.Error(), "column with this name already exists") {
					log.Infof("db: %s, table: %s error: %s", PROMETHEUS_DB, table, err)
				} else {
					return err
				}
			}
		}
	}
	return nil
}

func (w *PrometheusWriter) getCurrentAppLabelColumnCount() (int, error) {
	sql := fmt.Sprintf("SELECT count(0) FROM system.columns where database='%s' and table='%s' and name like '%%app_label_value%%'", PROMETHEUS_DB, PROMETHEUS_TABLE)
	log.Info(sql)
	rows, err := w.ckdbConn.Query(sql)
	if err != nil {
		w.ckdbConn = nil
		return 0, err
	}
	var count int
	for rows.Next() {
		err := rows.Scan(&count)
		if err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (w *PrometheusWriter) GetCounter() interface{} {
	var counter *Counter
	counter, w.counter = w.counter, &Counter{}
	return counter
}

// This function can be called when the FlowTags in the batch are the same (e.g. Prometheus metrics).
func (w *PrometheusWriter) WriteBatch(batch []interface{}, metricName string, timeSeries *prompb.TimeSeries, tsLabelNameIDs, tsLabelValueIDs []uint32) {
	if len(batch) == 0 {
		return
	}

	// Only the FlowTag in the first item needs to be written.
	prometheusMetrics := batch[0].(*PrometheusSample)
	ckwriter, err := w.getOrCreateCkwriter(prometheusMetrics)
	if err != nil {
		if w.counter.WriteErr == 0 {
			log.Warningf("get writer failed: %s", err)
		}
		atomic.AddInt64(&w.counter.WriteErr, 1)
		return
	}
	prometheusMetrics.GenerateNewFlowTags(w.flowTagWriter.Cache, metricName, timeSeries, tsLabelNameIDs, tsLabelValueIDs)
	w.flowTagWriter.WriteFieldsAndFieldValuesInCache()

	atomic.AddInt64(&w.counter.MetricsCount, int64(len(batch)))
	ckwriter.Put(batch...)
}

func NewPrometheusWriter(
	decoderIndex int,
	name string,
	db string,
	config *config.Config) (*PrometheusWriter, error) {

	// adjust CKWriterConfig
	ckWriterConfig := config.CKWriterConfig

	// FlowTagWriter
	flowTagWriterConfig := baseconfig.CKWriterConfig{
		QueueCount:   1,                        // Allocate one FlowTagWriter for each PrometheusWriter.
		QueueSize:    ckWriterConfig.QueueSize, // Only new FlowTags will be written, so the same QueueSize is used here.
		BatchSize:    ckWriterConfig.BatchSize,
		FlushTimeout: ckWriterConfig.FlushTimeout,
	}
	flowTagWriter, err := flow_tag.NewFlowTagWriter(decoderIndex, name, db, config.TTL, DefaultPartition, config.Base, &flowTagWriterConfig)
	if err != nil {
		return nil, err
	}

	// PrometheusWriter
	writer := &PrometheusWriter{
		decoderIndex:            decoderIndex,
		name:                    name,
		ckdbAddrs:               config.Base.CKDB.ActualAddrs,
		ckdbUsername:            config.Base.CKDBAuth.Username,
		ckdbPassword:            config.Base.CKDBAuth.Password,
		ckdbCluster:             config.Base.CKDB.ClusterName,
		ckdbStoragePolicy:       config.Base.CKDB.StoragePolicy,
		ckdbColdStorages:        config.Base.GetCKDBColdStorages(),
		ckdbTimeZone:            config.Base.CKDB.TimeZone,
		ttl:                     config.TTL,
		ckdbWatcher:             config.Base.CKDB.Watcher,
		writerConfig:            ckWriterConfig,
		flowTagWriter:           flowTagWriter,
		appLabelColumnIncrement: config.AppLabelColumnIncrement,

		counter: &Counter{},
	}
	if err := writer.InitTable(); err != nil {
		return nil, err
	}
	common.RegisterCountableForIngester("prometheus_writer", writer, stats.OptionStatTags{"msg": name, "decoder_index": strconv.Itoa(decoderIndex)})
	return writer, nil
}
