package sqleng

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/tsdb/interval"
	"xorm.io/core"
	"xorm.io/xorm"
)

// MetaKeyExecutedQueryString is the key where the executed query should get stored
const MetaKeyExecutedQueryString = "executedQueryString"

var ErrConnectionFailed = errors.New("failed to connect to server - please inspect Grafana server log for details")

// SQLMacroEngine interpolates macros into sql. It takes in the Query to have access to query context and
// timeRange to be able to generate queries that use from and to.
type SQLMacroEngine interface {
	Interpolate(query plugins.DataSubQuery, timeRange plugins.DataTimeRange, sql string) (string, error)
}

// SqlQueryResultTransformer transforms a query result row to RowValues with proper types.
type SqlQueryResultTransformer interface {
	// TransformQueryError transforms a query error.
	TransformQueryError(err error) error

	GetConverterList() []sqlutil.StringConverter
}

type engineCacheType struct {
	cache    map[int64]*xorm.Engine
	versions map[int64]int
	sync.Mutex
}

var engineCache = engineCacheType{
	cache:    make(map[int64]*xorm.Engine),
	versions: make(map[int64]int),
}

var sqlIntervalCalculator = interval.NewCalculator()

// NewXormEngine is an xorm.Engine factory, that can be stubbed by tests.
//nolint:gocritic
var NewXormEngine = func(driverName string, connectionString string) (*xorm.Engine, error) {
	return xorm.NewEngine(driverName, connectionString)
}

type dataPlugin struct {
	macroEngine            SQLMacroEngine
	queryResultTransformer SqlQueryResultTransformer
	engine                 *xorm.Engine
	timeColumnNames        []string
	metricColumnTypes      []string
	log                    log.Logger
}

type DataPluginConfiguration struct {
	DriverName        string
	Datasource        *models.DataSource
	ConnectionString  string
	TimeColumnNames   []string
	MetricColumnTypes []string
}

func (e *dataPlugin) transformQueryError(err error) error {
	// OpError is the error type usually returned by functions in the net
	// package. It describes the operation, network type, and address of
	// an error. We log this error rather than return it to the client
	// for security purposes.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		e.log.Error("query error", "err", err)
		return ErrConnectionFailed
	}

	return e.queryResultTransformer.TransformQueryError(err)
}

// NewDataPlugin returns a new plugins.DataPlugin
func NewDataPlugin(config DataPluginConfiguration, queryResultTransformer SqlQueryResultTransformer,
	macroEngine SQLMacroEngine, log log.Logger) (plugins.DataPlugin, error) {
	plugin := dataPlugin{
		queryResultTransformer: queryResultTransformer,
		macroEngine:            macroEngine,
		timeColumnNames:        []string{"time"},
		log:                    log,
	}

	if len(config.TimeColumnNames) > 0 {
		plugin.timeColumnNames = config.TimeColumnNames
	}

	if len(config.MetricColumnTypes) > 0 {
		plugin.metricColumnTypes = config.MetricColumnTypes
	}

	engineCache.Lock()
	defer engineCache.Unlock()

	if engine, present := engineCache.cache[config.Datasource.Id]; present {
		if version := engineCache.versions[config.Datasource.Id]; version == config.Datasource.Version {
			plugin.engine = engine
			return &plugin, nil
		}
	}

	engine, err := NewXormEngine(config.DriverName, config.ConnectionString)
	if err != nil {
		return nil, err
	}

	maxOpenConns := config.Datasource.JsonData.Get("maxOpenConns").MustInt(0)
	engine.SetMaxOpenConns(maxOpenConns)
	maxIdleConns := config.Datasource.JsonData.Get("maxIdleConns").MustInt(2)
	engine.SetMaxIdleConns(maxIdleConns)
	connMaxLifetime := config.Datasource.JsonData.Get("connMaxLifetime").MustInt(14400)
	engine.SetConnMaxLifetime(time.Duration(connMaxLifetime) * time.Second)

	engineCache.versions[config.Datasource.Id] = config.Datasource.Version
	engineCache.cache[config.Datasource.Id] = engine
	plugin.engine = engine

	return &plugin, nil
}

const rowLimit = 1000000

// DataQuery queries for data.
func (e *dataPlugin) DataQuery(ctx context.Context, dsInfo *models.DataSource,
	queryContext plugins.DataQuery) (plugins.DataResponse, error) {
	ch := make(chan plugins.DataQueryResult, len(queryContext.Queries))
	var wg sync.WaitGroup
	// Execute each query in a goroutine and wait for them to finish afterwards
	for _, query := range queryContext.Queries {
		if query.Model.Get("rawSql").MustString() == "" {
			continue
		}

		wg.Add(1)
		go e.executeQuery(query, &wg, queryContext, ch)
	}

	wg.Wait()

	// Read results from channels
	close(ch)
	result := plugins.DataResponse{
		Results: make(map[string]plugins.DataQueryResult),
	}
	for queryResult := range ch {
		result.Results[queryResult.RefID] = queryResult
	}

	return result, nil
}

func (e *dataPlugin) executeQuery(query plugins.DataSubQuery, wg *sync.WaitGroup, queryContext plugins.DataQuery,
	ch chan plugins.DataQueryResult) {
	defer wg.Done()

	queryResult := plugins.DataQueryResult{
		Meta:  simplejson.New(),
		RefID: query.RefID,
	}

	rawSQL := query.Model.Get("rawSql").MustString()
	if rawSQL == "" {
		panic("Query model property rawSql should not be empty at this point")
	}
	var timeRange plugins.DataTimeRange
	if queryContext.TimeRange != nil {
		timeRange = *queryContext.TimeRange
	}

	// global substitutions
	interpolatedQuery, err := Interpolate(query, timeRange, rawSQL)
	if err != nil {
		queryResult.Error = err
		ch <- queryResult
		return
	}

	// data source specific substitutions
	interpolatedQuery, err = e.macroEngine.Interpolate(query, timeRange, rawSQL)
	if err != nil {
		queryResult.Error = err
		ch <- queryResult
		return
	}

	backend.Logger.Debug("SQL query after interpolation", "sqlQuery", interpolatedQuery)

	errAppendDebug := func(frameErr string, err error) {
		var emptyFrame data.Frame
		emptyFrame.SetMeta(&data.FrameMeta{
			ExecutedQueryString: interpolatedQuery,
		})
		queryResult.Error = fmt.Errorf("%s: %w", frameErr, err)
		queryResult.Dataframes = plugins.NewDecodedDataFrames(data.Frames{&emptyFrame})
	}
	session := e.engine.NewSession()
	defer session.Close()
	db := session.DB()

	rows, err := db.Query(interpolatedQuery)
	if err != nil {
		errAppendDebug("db query error", e.transformQueryError(err))
		ch <- queryResult
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			e.log.Warn("Failed to close rows", "err", err)
		}
	}()

	qm, err := e.newProcessCfg(query, queryContext, rows, interpolatedQuery)
	if err != nil {
		errAppendDebug("failed to get configurations", err)
		return
	}

	// Convert row.Rows to dataframe
	myCs := e.queryResultTransformer.GetConverterList()
	frame, _, err := sqlutil.FrameFromRows(rows.Rows, rowLimit, myCs...)
	if err != nil {
		errAppendDebug("db query error", err)
		return
	}

	frame.SetMeta(&data.FrameMeta{
		ExecutedQueryString: interpolatedQuery,
	})

	backend.Logger.Debug("SQL query result row number", "queryResult", frame.Fields[0].Len())

	// If no rows were returned, no point checking anything else.
	if frame.Rows() == 0 {
		return
	}

	if qm.timeIndex != -1 {
		if err := convertSQLTimeColumnToEpochMS(frame, qm.timeIndex); err != nil {
			errAppendDebug("db convert time column failed", err)
			ch <- queryResult
			return
		}
	}

	if qm.Format == dataQueryFormatSeries {
		// time series has to have time column
		if qm.timeIndex == -1 {
			errAppendDebug("db has no time column", errors.New("no time column found"))
			ch <- queryResult
			return
		}
		for i := range qm.columnNames {
			if i == qm.timeIndex || i == qm.metricIndex {
				continue
			}

			var err error
			if frame, err = convertSQLValueColumnToFloat(frame, i); err != nil {
				errAppendDebug("convert value to float failed", err)
				ch <- queryResult
				return
			}
		}

		tsSchema := frame.TimeSeriesSchema()
		backend.Logger.Debug("Timeseries schema", "schema", tsSchema.Type)

		if tsSchema.Type == data.TimeSeriesTypeLong {
			var err error
			frame, err = data.LongToWide(frame, qm.FillMissing)
			if err != nil {
				errAppendDebug("failed to convert long to wide series when converting from dataframe", err)
				ch <- queryResult
				return
			}
		}
		if qm.FillMissing != nil {
			var err error
			frame, err = resample(frame, *qm)
			if err != nil {
				backend.Logger.Debug("Failed to resample dataframe", "err", err)
				frame.AppendNotices(data.Notice{Text: "Failed to resample dataframe", Severity: data.NoticeSeverityWarning})
			}
			if err := trim(frame, *qm); err != nil {
				backend.Logger.Debug("Failed to trim dataframe", "err", err)
				frame.AppendNotices(data.Notice{Text: "Failed to trim dataframe", Severity: data.NoticeSeverityWarning})
			}
		}
	}

	queryResult.Dataframes = plugins.NewDecodedDataFrames(data.Frames{frame})
	ch <- queryResult
}

// Interpolate provides global macros/substitutions for all sql datasources.
var Interpolate = func(query plugins.DataSubQuery, timeRange plugins.DataTimeRange, sql string) (string, error) {
	minInterval, err := interval.GetIntervalFrom(query.DataSource, query.Model, time.Second*60)
	if err != nil {
		return sql, err
	}
	interval := sqlIntervalCalculator.Calculate(timeRange, minInterval)

	sql = strings.ReplaceAll(sql, "$__interval_ms", strconv.FormatInt(interval.Milliseconds(), 10))
	sql = strings.ReplaceAll(sql, "$__interval", interval.Text)
	sql = strings.ReplaceAll(sql, "$__unixEpochFrom()", fmt.Sprintf("%d", timeRange.GetFromAsSecondsEpoch()))
	sql = strings.ReplaceAll(sql, "$__unixEpochTo()", fmt.Sprintf("%d", timeRange.GetToAsSecondsEpoch()))

	return sql, nil
}

func (e *dataPlugin) newProcessCfg(query plugins.DataSubQuery, queryContext plugins.DataQuery,
	rows *core.Rows, interpolatedQuery string) (*dataQueryModel, error) {
	columnNames, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}

	qm := &dataQueryModel{
		columnTypes:  columnTypes,
		columnNames:  columnNames,
		rows:         rows,
		timeIndex:    -1,
		metricIndex:  -1,
		metricPrefix: false,
		queryContext: queryContext,
	}

	if query.Model.Get("fill").MustBool(false) {
		qm.FillMissing = &data.FillMissing{}
		qm.Interval = time.Duration(query.Model.Get("fillInterval").MustFloat64() * float64(time.Second))
		switch strings.ToLower(query.Model.Get("fillMode").MustString()) {
		case "null":
			qm.FillMissing.Mode = data.FillModeNull
		case "previous":
			qm.FillMissing.Mode = data.FillModePrevious
		case "value":
			qm.FillMissing.Mode = data.FillModeValue
			qm.FillMissing.Value = query.Model.Get("fillValue").MustFloat64()
		default:
		}
	}

	if queryContext.TimeRange != nil {
		qm.TimeRange.From = queryContext.TimeRange.GetFromAsTimeUTC()
		qm.TimeRange.To = queryContext.TimeRange.GetToAsTimeUTC()
	}

	format := query.Model.Get("format").MustString("time_series")
	switch format {
	case "time_series":
		qm.Format = dataQueryFormatSeries
	case "table":
		qm.Format = dataQueryFormatTable
	default:
		panic(fmt.Sprintf("Unrecognized query model format: %q", format))
	}

	for i, col := range qm.columnNames {
		for _, tc := range e.timeColumnNames {
			if col == tc {
				qm.timeIndex = i
				break
			}
		}
		switch col {
		case "metric":
			qm.metricIndex = i
		default:
			if qm.metricIndex == -1 {
				columnType := qm.columnTypes[i].DatabaseTypeName()
				for _, mct := range e.metricColumnTypes {
					if columnType == mct {
						qm.metricIndex = i
						continue
					}
				}
			}
		}
	}
	qm.InterpolatedQuery = interpolatedQuery
	return qm, nil
}

// dataQueryFormat is the type of query.
type dataQueryFormat string

const (
	// dataQueryFormatTable identifies a table query (default).
	dataQueryFormatTable dataQueryFormat = "table"
	// dataQueryFormatSeries identifies a time series query.
	dataQueryFormatSeries dataQueryFormat = "time_series"
)

type dataQueryModel struct {
	InterpolatedQuery string // property non set until after Interpolate()
	Format            dataQueryFormat
	TimeRange         backend.TimeRange
	FillMissing       *data.FillMissing // property non set until after Interpolate()
	Interval          time.Duration
	columnNames       []string
	columnTypes       []*sql.ColumnType
	timeIndex         int
	metricIndex       int
	rows              *core.Rows
	metricPrefix      bool
	queryContext      plugins.DataQuery
}

func convertInt64ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(int64))
		newField.Append(&value)
	}
}

func convertNullableInt64ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int64)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertUInt64ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(uint64))
		newField.Append(&value)
	}
}

func convertNullableUInt64ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint64)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertInt32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(int32))
		newField.Append(&value)
	}
}

func convertNullableInt32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertUInt32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(uint32))
		newField.Append(&value)
	}
}

func convertNullableUInt32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertInt16ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(int16))
		newField.Append(&value)
	}
}

func convertNullableInt16ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int16)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertUInt16ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(uint16))
		newField.Append(&value)
	}
}

func convertNullableUInt16ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint16)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertInt8ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(int8))
		newField.Append(&value)
	}
}

func convertNullableInt8ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int8)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertUInt8ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(uint8))
		newField.Append(&value)
	}
}

func convertNullableUInt8ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint8)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertUnknownToZero(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(0)
		newField.Append(&value)
	}
}

func convertNullableFloat32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*float32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := float64(*iv)
			newField.Append(&value)
		}
	}
}

func convertFloat32ToFloat64(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := float64(origin.At(i).(float32))
		newField.Append(&value)
	}
}

func convertInt64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(float64(origin.At(i).(int64))))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableInt64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int64)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(float64(*iv)))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

func convertUInt64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(float64(origin.At(i).(uint64))))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableUInt64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint64)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(float64(*iv)))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

func convertInt32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(float64(origin.At(i).(int32))))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableInt32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*int32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(float64(*iv)))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

func convertUInt32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(float64(origin.At(i).(uint32))))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableUInt32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*uint32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(float64(*iv)))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

func convertFloat64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(origin.At(i).(float64)))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableFloat64ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*float64)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(*iv))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

func convertFloat32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		value := time.Unix(0, int64(epochPrecisionToMS(float64(origin.At(i).(float32))))*int64(time.Millisecond))
		newField.Append(&value)
	}
}

func convertNullableFloat32ToEpochMS(origin *data.Field, newField *data.Field) {
	valueLength := origin.Len()
	for i := 0; i < valueLength; i++ {
		iv := origin.At(i).(*float32)
		if iv == nil {
			newField.Append(nil)
		} else {
			value := time.Unix(0, int64(epochPrecisionToMS(float64(*iv)))*int64(time.Millisecond))
			newField.Append(&value)
		}
	}
}

// convertSQLTimeColumnToEpochMS converts column named time to unix timestamp in milliseconds
// to make native datetime types and epoch dates work in annotation and table queries.
func convertSQLTimeColumnToEpochMS(frame *data.Frame, timeIndex int) error {
	if timeIndex < 0 || timeIndex >= len(frame.Fields) {
		return fmt.Errorf("timeIndex %d is out of range", timeIndex)
	}

	origin := frame.Fields[timeIndex]
	valueType := origin.Type()
	if valueType == data.FieldTypeTime || valueType == data.FieldTypeNullableTime {
		return nil
	}

	newField := data.NewFieldFromFieldType(data.FieldTypeNullableTime, 0)
	newField.Name = origin.Name
	newField.Labels = origin.Labels

	switch valueType {
	case data.FieldTypeInt64:
		convertInt64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableInt64:
		convertNullableInt64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeUint64:
		convertUInt64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableUint64:
		convertNullableUInt64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeInt32:
		convertInt32ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableInt32:
		convertNullableInt32ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeUint32:
		convertUInt32ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableUint32:
		convertNullableUInt32ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeFloat64:
		convertFloat64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableFloat64:
		convertNullableFloat64ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeFloat32:
		convertFloat32ToEpochMS(frame.Fields[timeIndex], newField)
	case data.FieldTypeNullableFloat32:
		convertNullableFloat32ToEpochMS(frame.Fields[timeIndex], newField)
	default:
		return fmt.Errorf("column type %q is not convertible to time.Time", valueType)
	}
	frame.Fields[timeIndex] = newField

	return nil
}

// convertSQLValueColumnToFloat converts timeseries value column to float.
//nolint: gocyclo
func convertSQLValueColumnToFloat(frame *data.Frame, Index int) (*data.Frame, error) {
	if Index < 0 || Index >= len(frame.Fields) {
		return frame, fmt.Errorf("metricIndex %d is out of range", Index)
	}

	origin := frame.Fields[Index]
	valueType := origin.Type()
	if valueType == data.FieldTypeFloat64 || valueType == data.FieldTypeNullableFloat64 {
		return frame, nil
	}

	newField := data.NewFieldFromFieldType(data.FieldTypeNullableFloat64, 0)
	newField.Name = origin.Name
	newField.Labels = origin.Labels

	backend.Logger.Info("SQL column type converting to float64", "sqlConvertFloat64", valueType)
	switch valueType {
	case data.FieldTypeInt64:
		convertInt64ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableInt64:
		convertNullableInt64ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeUint64:
		convertUInt64ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableUint64:
		convertNullableUInt64ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeInt32:
		convertInt32ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableInt32:
		convertNullableInt32ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeUint32:
		convertUInt32ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableUint32:
		convertNullableUInt32ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeInt16:
		convertInt16ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableInt16:
		convertNullableInt16ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeUint16:
		convertUInt16ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableUint16:
		convertNullableUInt16ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeInt8:
		convertInt8ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableInt8:
		convertNullableInt8ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeUint8:
		convertUInt8ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableUint8:
		convertNullableUInt8ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeFloat32:
		convertFloat32ToFloat64(frame.Fields[Index], newField)
	case data.FieldTypeNullableFloat32:
		convertNullableFloat32ToFloat64(frame.Fields[Index], newField)
	default:
		convertUnknownToZero(frame.Fields[Index], newField)
		frame.Fields[Index] = newField
		return frame, fmt.Errorf("metricIndex %d type can't be converted to float", Index)
	}
	frame.Fields[Index] = newField

	return frame, nil
}

func SetupFillmode(query plugins.DataSubQuery, interval time.Duration, fillmode string) error {
	query.Model.Set("fill", true)
	query.Model.Set("fillInterval", interval.Seconds())
	switch fillmode {
	case "NULL":
		query.Model.Set("fillMode", "null")
	case "previous":
		query.Model.Set("fillMode", "previous")
	default:
		query.Model.Set("fillMode", "value")
		floatVal, err := strconv.ParseFloat(fillmode, 64)
		if err != nil {
			return fmt.Errorf("error parsing fill value %v", fillmode)
		}
		query.Model.Set("fillValue", floatVal)
	}

	return nil
}

type SQLMacroEngineBase struct{}

func NewSQLMacroEngineBase() *SQLMacroEngineBase {
	return &SQLMacroEngineBase{}
}

func (m *SQLMacroEngineBase) ReplaceAllStringSubmatchFunc(re *regexp.Regexp, str string, repl func([]string) string) string {
	result := ""
	lastIndex := 0

	for _, v := range re.FindAllSubmatchIndex([]byte(str), -1) {
		groups := []string{}
		for i := 0; i < len(v); i += 2 {
			groups = append(groups, str[v[i]:v[i+1]])
		}

		result += str[lastIndex:v[0]] + repl(groups)
		lastIndex = v[1]
	}

	return result + str[lastIndex:]
}

// epochPrecisionToMS converts epoch precision to millisecond, if needed.
// Only seconds to milliseconds supported right now
func epochPrecisionToMS(value float64) float64 {
	s := strconv.FormatFloat(value, 'e', -1, 64)
	if strings.HasSuffix(s, "e+09") {
		return value * float64(1e3)
	}

	if strings.HasSuffix(s, "e+18") {
		return value / float64(time.Millisecond)
	}

	return value
}
