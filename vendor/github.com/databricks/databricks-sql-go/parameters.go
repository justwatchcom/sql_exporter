package dbsql

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/databricks/databricks-sql-go/internal/cli_service"
)

type Parameter struct {
	Name  string
	Type  SqlType
	Value any
}

type SqlType int

const (
	SqlUnkown SqlType = iota
	SqlString
	SqlDate
	SqlTimestamp
	SqlFloat
	SqlDecimal
	SqlDouble
	SqlInteger
	SqlBigInt
	SqlSmallInt
	SqlTinyInt
	SqlBoolean
	SqlIntervalMonth
	SqlIntervalDay
	SqlVoid
)

func (s SqlType) String() string {
	switch s {
	case SqlString:
		return "STRING"
	case SqlDate:
		return "DATE"
	case SqlTimestamp:
		return "TIMESTAMP"
	case SqlFloat:
		return "FLOAT"
	case SqlDecimal:
		return "DECIMAL"
	case SqlDouble:
		return "DOUBLE"
	case SqlInteger:
		return "INTEGER"
	case SqlBigInt:
		return "BIGINT"
	case SqlSmallInt:
		return "SMALLINT"
	case SqlTinyInt:
		return "TINYINT"
	case SqlBoolean:
		return "BOOLEAN"
	case SqlIntervalMonth:
		return "INTERVAL MONTH"
	case SqlIntervalDay:
		return "INTERVAL DAY"
	case SqlVoid:
		return "VOID"
	}
	return "unknown"
}

func valuesToParameters(namedValues []driver.NamedValue) []Parameter {
	var params []Parameter
	for i := range namedValues {
		newParam := *new(Parameter)
		namedValue := namedValues[i]
		param, ok := namedValue.Value.(Parameter)
		if ok {
			newParam.Name = param.Name
			newParam.Value = param.Value
			newParam.Type = param.Type
		} else {
			newParam.Name = namedValue.Name
			newParam.Value = namedValue.Value
		}
		params = append(params, newParam)
	}
	return params
}

func inferTypes(params []Parameter) {
	for i := range params {
		param := &params[i]
		if param.Type == SqlUnkown {
			inferType(param)
		}
	}
}

func inferType(param *Parameter) {
	if param.Value != nil && reflect.ValueOf(param.Value).Kind() == reflect.Ptr {
		param.Value = reflect.ValueOf(param.Value).Elem().Interface()
		inferType(param)
		return
	}

	switch value := param.Value.(type) {
	case bool:
		param.Value = strconv.FormatBool(value)
		param.Type = SqlBoolean
	case string:
		param.Value = value
		param.Type = SqlString
	case int:
		param.Value = strconv.Itoa(value)
		param.Type = SqlInteger
	case uint:
		param.Value = strconv.FormatUint(uint64(value), 10)
		param.Type = SqlInteger
	case int8:
		param.Value = strconv.Itoa(int(value))
		param.Type = SqlInteger
	case uint8:
		param.Value = strconv.FormatUint(uint64(value), 10)
		param.Type = SqlInteger
	case int16:
		param.Value = strconv.Itoa(int(value))
		param.Type = SqlInteger
	case uint16:
		param.Value = strconv.FormatUint(uint64(value), 10)
		param.Type = SqlInteger
	case int32:
		param.Value = strconv.Itoa(int(value))
		param.Type = SqlInteger
	case uint32:
		param.Value = strconv.FormatUint(uint64(value), 10)
		param.Type = SqlInteger
	case int64:
		param.Value = strconv.Itoa(int(value))
		param.Type = SqlInteger
	case uint64:
		param.Value = strconv.FormatUint(uint64(value), 10)
		param.Type = SqlInteger
	case float32:
		param.Value = strconv.FormatFloat(float64(value), 'f', -1, 32)
		param.Type = SqlFloat
	case float64:
		param.Value = strconv.FormatFloat(float64(value), 'f', -1, 64)
		param.Type = SqlFloat
	case time.Time:
		param.Value = value.Format(time.RFC3339Nano)
		param.Type = SqlTimestamp
	case nil:
		param.Value = nil
		param.Type = SqlVoid
	default:
		s := fmt.Sprintf("%s", param.Value)
		param.Value = s
		param.Type = SqlString
	}
}

func convertNamedValuesToSparkParams(values []driver.NamedValue) []*cli_service.TSparkParameter {
	var sparkParams []*cli_service.TSparkParameter

	sqlParams := valuesToParameters(values)
	inferTypes(sqlParams)
	for i := range sqlParams {
		sqlParam := sqlParams[i]
		sparkValue := new(cli_service.TSparkParameterValue)
		if sqlParam.Type == SqlVoid {
			sparkValue = nil
		} else {
			stringValue := sqlParam.Value.(string)
			sparkValue = &cli_service.TSparkParameterValue{StringValue: &stringValue}
		}

		var sparkParamType string
		if sqlParam.Type == SqlDecimal {
			sparkParamType = inferDecimalType(sparkValue.GetStringValue())
		} else {
			sparkParamType = sqlParam.Type.String()
		}
		sparkParam := cli_service.TSparkParameter{Name: &sqlParam.Name, Type: &sparkParamType, Value: sparkValue}
		sparkParams = append(sparkParams, &sparkParam)
	}
	return sparkParams
}

func inferDecimalType(d string) (t string) {
	var overall int
	var after int
	if strings.HasPrefix(d, "0.") {
		// Less than one
		overall = len(d) - 2
		after = len(d) - 2
	} else if !strings.Contains(d, ".") {
		// Less than one
		overall = len(d)
		after = 0
	} else {
		components := strings.Split(d, ".")
		overall, after = len(components[0])+len(components[1]), len(components[1])
	}

	return fmt.Sprintf("DECIMAL(%d,%d)", overall, after)
}
