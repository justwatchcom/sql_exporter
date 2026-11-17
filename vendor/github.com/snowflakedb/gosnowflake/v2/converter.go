package gosnowflake

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	errors2 "github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"github.com/snowflakedb/gosnowflake/v2/internal/query"
	"github.com/snowflakedb/gosnowflake/v2/internal/types"
	"github.com/snowflakedb/gosnowflake/v2/sflog"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	ia "github.com/snowflakedb/gosnowflake/v2/internal/arrow"
)

const format = "2006-01-02 15:04:05.999999999"
const streamTimestampFormat = "2006-01-02 15:04:05.000000000 Z07:00"
const numberDefaultPrecision = 38
const jsonFormatStr = "json"

const numberMaxPrecisionInBits = 127

// 38 (max precision) + 1 (for possible '-') + 1 (for possible '.')
const decfloatPrintingPrec = 40

type timezoneType int

var errUnsupportedTimeArrayBind = errors.New("unsupported time array bind. Set the type to TimestampNTZType, TimestampLTZType, TimestampTZType, DateType or TimeType")
var errNativeArrowWithoutProperContext = errors.New("structured types must be enabled to use with native arrow")

const (
	// TimestampNTZType denotes a NTZ timezoneType for array binds
	TimestampNTZType timezoneType = iota
	// TimestampLTZType denotes a LTZ timezoneType for array binds
	TimestampLTZType
	// TimestampTZType denotes a TZ timezoneType for array binds
	TimestampTZType
	// DateType denotes a date type for array binds
	DateType
	// TimeType denotes a time type for array binds
	TimeType
)

type interfaceArrayBinding struct {
	hasTimezone       bool
	tzType            timezoneType
	timezoneTypeArray any
}

func isInterfaceArrayBinding(t any) bool {
	switch t.(type) {
	case interfaceArrayBinding:
		return true
	case *interfaceArrayBinding:
		return true
	default:
		return false
	}
}

func isJSONFormatType(tsmode types.SnowflakeType) bool {
	return tsmode == types.ObjectType || tsmode == types.ArrayType || tsmode == types.SliceType
}

// goTypeToSnowflake translates Go data type to Snowflake data type.
func goTypeToSnowflake(v driver.Value, tsmode types.SnowflakeType) types.SnowflakeType {
	if isJSONFormatType(tsmode) {
		return tsmode
	}
	if v == nil {
		return types.NullType
	}
	switch t := v.(type) {
	case int64, sql.NullInt64:
		return types.FixedType
	case float64, sql.NullFloat64:
		return types.RealType
	case bool, sql.NullBool:
		return types.BooleanType
	case string, sql.NullString:
		return types.TextType
	case []byte:
		if tsmode == types.BinaryType {
			return types.BinaryType // may be redundant but ensures BINARY type
		}
		if t == nil {
			return types.NullType // invalid byte array. won't take as BINARY
		}
		if len(t) != 1 {
			return types.ArrayType
		}
		if _, err := dataTypeMode(t); err != nil {
			return types.UnSupportedType
		}
		return types.ChangeType
	case time.Time, sql.NullTime:
		return tsmode
	}
	if supportedArrayBind(&driver.NamedValue{Value: v}) {
		return types.SliceType
	}
	// structured objects
	if _, ok := v.(StructuredObjectWriter); ok {
		return types.ObjectType
	} else if _, ok := v.(reflect.Type); ok && tsmode == types.NilObjectType {
		return types.NilObjectType
	}
	// structured arrays
	if reflect.TypeOf(v).Kind() == reflect.Slice || (reflect.TypeOf(v).Kind() == reflect.Pointer && reflect.ValueOf(v).Elem().Kind() == reflect.Slice) {
		return types.ArrayType
	} else if tsmode == types.NilArrayType {
		return types.NilArrayType
	} else if tsmode == types.NilMapType {
		return types.NilMapType
	} else if reflect.TypeOf(v).Kind() == reflect.Map || (reflect.TypeOf(v).Kind() == reflect.Pointer && reflect.ValueOf(v).Elem().Kind() == reflect.Map) {
		return types.MapType
	}
	return types.UnSupportedType
}

// snowflakeTypeToGo translates Snowflake data type to Go data type.
func snowflakeTypeToGo(ctx context.Context, dbtype types.SnowflakeType, precision int64, scale int64, fields []query.FieldMetadata) reflect.Type {
	structuredTypesEnabled := structuredTypesEnabled(ctx)
	switch dbtype {
	case types.FixedType:
		if higherPrecisionEnabled(ctx) {
			if scale == 0 {
				if precision >= 19 {
					return reflect.TypeFor[*big.Int]()
				}
				return reflect.TypeFor[int64]()
			}
			return reflect.TypeFor[*big.Float]()
		}
		if scale == 0 {
			if precision >= 19 {
				return reflect.TypeFor[string]()
			}
			return reflect.TypeFor[int64]()
		}
		return reflect.TypeFor[float64]()
	case types.RealType:
		return reflect.TypeFor[float64]()
	case types.DecfloatType:
		if !decfloatMappingEnabled(ctx) {
			return reflect.TypeFor[string]()
		}
		if higherPrecisionEnabled(ctx) {
			return reflect.TypeFor[*big.Float]()
		}
		return reflect.TypeFor[float64]()
	case types.TextType, types.VariantType:
		return reflect.TypeFor[string]()
	case types.DateType, types.TimeType, types.TimestampLtzType, types.TimestampNtzType, types.TimestampTzType:
		return reflect.TypeOf(time.Now())
	case types.BinaryType:
		return reflect.TypeFor[[]byte]()
	case types.BooleanType:
		return reflect.TypeFor[bool]()
	case types.ObjectType:
		if len(fields) > 0 && structuredTypesEnabled {
			return reflect.TypeFor[ObjectType]()
		}
		return reflect.TypeFor[string]()
	case types.ArrayType:
		if len(fields) == 0 || !structuredTypesEnabled {
			return reflect.TypeFor[string]()
		}
		if len(fields) != 1 {
			logger.WithContext(ctx).Warn("Unexpected fields number: " + strconv.Itoa(len(fields)))
			return reflect.TypeFor[string]()
		}
		switch types.GetSnowflakeType(fields[0].Type) {
		case types.FixedType:
			if fields[0].Scale == 0 && higherPrecisionEnabled(ctx) {
				return reflect.TypeFor[[]*big.Int]()
			} else if fields[0].Scale == 0 && !higherPrecisionEnabled(ctx) {
				return reflect.TypeFor[[]int64]()
			} else if fields[0].Scale != 0 && higherPrecisionEnabled(ctx) {
				return reflect.TypeFor[[]*big.Float]()
			}
			return reflect.TypeFor[[]float64]()
		case types.RealType:
			return reflect.TypeFor[[]float64]()
		case types.TextType:
			return reflect.TypeFor[[]string]()
		case types.DateType, types.TimeType, types.TimestampLtzType, types.TimestampNtzType, types.TimestampTzType:
			return reflect.TypeFor[[]time.Time]()
		case types.BooleanType:
			return reflect.TypeFor[[]bool]()
		case types.BinaryType:
			return reflect.TypeFor[[][]byte]()
		case types.ObjectType:
			return reflect.TypeFor[[]ObjectType]()
		}
		return nil
	case types.MapType:
		if !structuredTypesEnabled {
			return reflect.TypeFor[string]()
		}
		switch types.GetSnowflakeType(fields[0].Type) {
		case types.TextType:
			return snowflakeTypeToGoForMaps[string](ctx, fields[1])
		case types.FixedType:
			return snowflakeTypeToGoForMaps[int64](ctx, fields[1])
		}
		return reflect.TypeFor[map[any]any]()
	}
	logger.WithContext(ctx).Errorf("unsupported dbtype is specified. %v", dbtype)
	return reflect.TypeFor[string]()
}

func snowflakeTypeToGoForMaps[K comparable](ctx context.Context, valueMetadata query.FieldMetadata) reflect.Type {
	switch types.GetSnowflakeType(valueMetadata.Type) {
	case types.TextType:
		return reflect.TypeFor[map[K]string]()
	case types.FixedType:
		if higherPrecisionEnabled(ctx) && valueMetadata.Scale == 0 {
			return reflect.TypeFor[map[K]*big.Int]()
		} else if higherPrecisionEnabled(ctx) && valueMetadata.Scale != 0 {
			return reflect.TypeFor[map[K]*big.Float]()
		} else if !higherPrecisionEnabled(ctx) && valueMetadata.Scale == 0 {
			return reflect.TypeFor[map[K]int64]()
		} else {
			return reflect.TypeFor[map[K]float64]()
		}
	case types.RealType:
		return reflect.TypeFor[map[K]float64]()
	case types.BooleanType:
		return reflect.TypeFor[map[K]bool]()
	case types.BinaryType:
		return reflect.TypeFor[map[K][]byte]()
	case types.TimeType, types.DateType, types.TimestampTzType, types.TimestampNtzType, types.TimestampLtzType:
		return reflect.TypeFor[map[K]time.Time]()
	}
	logger.WithContext(ctx).Errorf("unsupported dbtype is specified for map value")
	return reflect.TypeFor[string]()
}

// valueToString converts arbitrary golang type to a string. This is mainly used in binding data with placeholders
// in queries.
func valueToString(v driver.Value, tsmode types.SnowflakeType, params *syncParams) (bindingValue, error) {
	isJSONFormat := isJSONFormatType(tsmode)
	if v == nil {
		if isJSONFormat {
			return bindingValue{nil, jsonFormatStr, nil}, nil
		}
		return bindingValue{nil, "", nil}, nil
	}
	v1 := reflect.Indirect(reflect.ValueOf(v))

	if valuer, ok := v.(driver.Valuer); ok { // check for driver.Valuer satisfaction and honor that first
		if value, err := valuer.Value(); err == nil && value != nil {
			// if the output value is a valid string, return that
			if strVal, ok := value.(string); ok {
				if isJSONFormat {
					return bindingValue{&strVal, jsonFormatStr, nil}, nil
				}
				return bindingValue{&strVal, "", nil}, nil
			}
		}
	}

	if tsmode == types.DecfloatType && v1.Type() == reflect.TypeFor[big.Float]() {
		s := v.(*big.Float).Text('g', decfloatPrintingPrec)
		return bindingValue{&s, "", nil}, nil
	}

	switch v1.Kind() {
	case reflect.Bool:
		s := strconv.FormatBool(v1.Bool())
		return bindingValue{&s, "", nil}, nil
	case reflect.Int64:
		s := strconv.FormatInt(v1.Int(), 10)
		return bindingValue{&s, "", nil}, nil
	case reflect.Float64:
		s := strconv.FormatFloat(v1.Float(), 'g', -1, 32)
		return bindingValue{&s, "", nil}, nil
	case reflect.String:
		s := v1.String()
		if isJSONFormat {
			return bindingValue{&s, jsonFormatStr, nil}, nil
		}
		return bindingValue{&s, "", nil}, nil
	case reflect.Slice, reflect.Array:
		return arrayToString(v, tsmode, params)
	case reflect.Map:
		return mapToString(v, tsmode, params)
	case reflect.Struct:
		return structValueToString(v, tsmode, params)
	}

	return bindingValue{}, fmt.Errorf("unsupported type: %v", v1.Kind())
}

// isUUIDImplementer checks if a value is a UUID that satisfies RFC 4122
func isUUIDImplementer(v reflect.Value) bool {
	rt := v.Type()

	// Check if the type is an array of 16 bytes
	if v.Kind() == reflect.Array && rt.Elem().Kind() == reflect.Uint8 && rt.Len() == 16 {
		// Check if the type implements fmt.Stringer
		vInt := v.Interface()
		if stringer, ok := vInt.(fmt.Stringer); ok {
			uuidStr := stringer.String()

			rfc4122Regex := `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
			matched, err := regexp.MatchString(rfc4122Regex, uuidStr)
			if err != nil {
				return false
			}

			if matched {
				// parse the UUID and ensure it is the same as the original string
				u := ParseUUID(uuidStr)
				return u.String() == uuidStr
			}
		}
	}
	return false
}

func arrayToString(v driver.Value, tsmode types.SnowflakeType, params *syncParams) (bindingValue, error) {
	v1 := reflect.Indirect(reflect.ValueOf(v))
	if v1.Kind() == reflect.Slice && v1.IsNil() {
		return bindingValue{nil, jsonFormatStr, nil}, nil
	}
	if bd, ok := v.([][]byte); ok && tsmode == types.BinaryType {
		schema := bindingSchema{
			Typ:      "array",
			Nullable: true,
			Fields: []query.FieldMetadata{
				{
					Type:     "binary",
					Nullable: true,
				},
			},
		}
		if len(bd) == 0 {
			res := "[]"
			return bindingValue{value: &res, format: jsonFormatStr, schema: &schema}, nil
		}
		s := ""
		for _, b := range bd {
			s += "\"" + hex.EncodeToString(b) + "\","
		}
		s = "[" + s[:len(s)-1] + "]"
		return bindingValue{&s, jsonFormatStr, &schema}, nil
	} else if times, ok := v.([]time.Time); ok {
		typ := types.DriverTypeToSnowflake[tsmode]
		sfFormat, err := dateTimeInputFormatByType(typ, params)
		if err != nil {
			return bindingValue{nil, "", nil}, err
		}
		goFormat, err := snowflakeFormatToGoFormat(sfFormat)
		if err != nil {
			return bindingValue{nil, "", nil}, err
		}
		arr := make([]string, len(times))
		for idx, t := range times {
			arr[idx] = t.Format(goFormat)
		}
		res, err := json.Marshal(arr)
		if err != nil {
			return bindingValue{nil, jsonFormatStr, &bindingSchema{
				Typ:      "array",
				Nullable: true,
				Fields: []query.FieldMetadata{
					{
						Type:     typ,
						Nullable: true,
					},
				},
			}}, err
		}
		resString := string(res)
		return bindingValue{&resString, jsonFormatStr, nil}, nil
	} else if isArrayOfStructs(v) {
		stringEntries := make([]string, v1.Len())
		sowcForSingleElement, err := buildSowcFromType(params, reflect.TypeOf(v).Elem())
		if err != nil {
			return bindingValue{}, err
		}
		for i := 0; i < v1.Len(); i++ {
			potentialSow := v1.Index(i)
			if sow, ok := potentialSow.Interface().(StructuredObjectWriter); ok {
				bv, err := structValueToString(sow, tsmode, params)
				if err != nil {
					return bindingValue{nil, jsonFormatStr, nil}, err
				}
				stringEntries[i] = *bv.value
			}
		}
		value := "[" + strings.Join(stringEntries, ",") + "]"
		arraySchema := &bindingSchema{
			Typ:      "array",
			Nullable: true,
			Fields: []query.FieldMetadata{
				{
					Type:     "OBJECT",
					Nullable: true,
					Fields:   sowcForSingleElement.toFields(),
				},
			},
		}
		return bindingValue{&value, jsonFormatStr, arraySchema}, nil
	} else if reflect.ValueOf(v).Len() == 0 {
		value := "[]"
		return bindingValue{&value, jsonFormatStr, nil}, nil
	} else if barr, ok := v.([]byte); ok {
		if tsmode == types.BinaryType {
			res := hex.EncodeToString(barr)
			return bindingValue{&res, jsonFormatStr, nil}, nil
		}
		schemaForBytes := bindingSchema{
			Typ:      "array",
			Nullable: true,
			Fields: []query.FieldMetadata{
				{
					Type:     "FIXED",
					Nullable: true,
				},
			},
		}
		if len(barr) == 0 {
			res := "[]"
			return bindingValue{&res, jsonFormatStr, &schemaForBytes}, nil
		}
		res := "["
		for _, b := range barr {
			res += strconv.FormatUint(uint64(b), 10) + ","
		}
		res = res[0:len(res)-1] + "]"
		return bindingValue{&res, jsonFormatStr, &schemaForBytes}, nil
	} else if isUUIDImplementer(v1) { // special case for UUIDs (snowflake type and other implementers)
		stringer := v.(fmt.Stringer) // we don't need to validate if it's a fmt.Stringer because we already checked if it's a UUID type with a stringer
		value := stringer.String()
		return bindingValue{&value, "", nil}, nil
	} else if isSliceOfSlices(v) {
		return bindingValue{}, errors.New("array of arrays is not supported")
	}
	res, err := json.Marshal(v)
	if err != nil {
		return bindingValue{nil, jsonFormatStr, nil}, err
	}
	resString := string(res)
	return bindingValue{&resString, jsonFormatStr, nil}, nil
}

func mapToString(v driver.Value, tsmode types.SnowflakeType, params *syncParams) (bindingValue, error) {
	var err error
	valOf := reflect.Indirect(reflect.ValueOf(v))
	if valOf.IsNil() {
		return bindingValue{nil, "", nil}, nil
	}
	typOf := reflect.TypeOf(v)
	var jsonBytes []byte
	if tsmode == types.BinaryType {
		m := make(map[string]*string, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val := iter.Value().Interface().([]byte)
			if val != nil {
				s := hex.EncodeToString(val)
				m[stringOrIntToString(iter.Key())] = &s
			} else {
				m[stringOrIntToString(iter.Key())] = nil
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(reflect.TypeFor[time.Time]()) || typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullTime]()) {
		m := make(map[string]*string, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val, valid, err := toNullableTime(iter.Value().Interface())
			if err != nil {
				return bindingValue{}, err
			}
			if !valid {
				m[stringOrIntToString(iter.Key())] = nil
			} else {
				typ := types.DriverTypeToSnowflake[tsmode]
				s, err := timeToString(val, typ, params)
				if err != nil {
					return bindingValue{}, err
				}
				m[stringOrIntToString(iter.Key())] = &s
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullString]()) {
		m := make(map[string]*string, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val := iter.Value().Interface().(sql.NullString)
			if val.Valid {
				m[stringOrIntToString(iter.Key())] = &val.String
			} else {
				m[stringOrIntToString(iter.Key())] = nil
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullByte]()) || typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullInt16]()) || typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullInt32]()) || typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullInt64]()) {
		m := make(map[string]*int64, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val, valid := toNullableInt64(iter.Value().Interface())
			if valid {
				m[stringOrIntToString(iter.Key())] = &val
			} else {
				m[stringOrIntToString(iter.Key())] = nil
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullFloat64]()) {
		m := make(map[string]*float64, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val := iter.Value().Interface().(sql.NullFloat64)
			if val.Valid {
				m[stringOrIntToString(iter.Key())] = &val.Float64
			} else {
				m[stringOrIntToString(iter.Key())] = nil
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(reflect.TypeFor[sql.NullBool]()) {
		m := make(map[string]*bool, valOf.Len())
		iter := valOf.MapRange()
		for iter.Next() {
			val := iter.Value().Interface().(sql.NullBool)
			if val.Valid {
				m[stringOrIntToString(iter.Key())] = &val.Bool
			} else {
				m[stringOrIntToString(iter.Key())] = nil
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
	} else if typOf.Elem().AssignableTo(structuredObjectWriterType) {
		m := make(map[string]map[string]any, valOf.Len())
		iter := valOf.MapRange()
		var valueMetadata *query.FieldMetadata
		for iter.Next() {
			sowc := structuredObjectWriterContext{}
			sowc.init(params)
			if iter.Value().IsNil() {
				m[stringOrIntToString(iter.Key())] = nil
				continue
			}
			err = iter.Value().Interface().(StructuredObjectWriter).Write(&sowc)
			if err != nil {
				return bindingValue{}, err
			}
			m[stringOrIntToString(iter.Key())] = sowc.values
			if valueMetadata == nil {
				valueMetadata = &query.FieldMetadata{
					Type:     "OBJECT",
					Nullable: true,
					Fields:   sowc.toFields(),
				}
			}
		}
		if valueMetadata == nil {
			sowcFromValueType, err := buildSowcFromType(params, typOf.Elem())
			if err != nil {
				return bindingValue{}, err
			}
			valueMetadata = &query.FieldMetadata{
				Type:     "OBJECT",
				Nullable: true,
				Fields:   sowcFromValueType.toFields(),
			}
		}
		jsonBytes, err = json.Marshal(m)
		if err != nil {
			return bindingValue{}, err
		}
		jsonString := string(jsonBytes)
		keyMetadata, err := goTypeToFieldMetadata(typOf.Key(), types.TextType, params)
		if err != nil {
			return bindingValue{}, err
		}
		schema := bindingSchema{
			Typ:    "MAP",
			Fields: []query.FieldMetadata{keyMetadata, *valueMetadata},
		}
		return bindingValue{&jsonString, jsonFormatStr, &schema}, nil
	} else {
		jsonBytes, err = json.Marshal(v)
		if err != nil {
			return bindingValue{}, err
		}
	}
	jsonString := string(jsonBytes)
	keyMetadata, err := goTypeToFieldMetadata(typOf.Key(), types.TextType, params)
	if err != nil {
		return bindingValue{}, err
	}
	valueMetadata, err := goTypeToFieldMetadata(typOf.Elem(), tsmode, params)
	if err != nil {
		return bindingValue{}, err
	}
	schema := bindingSchema{
		Typ:    "MAP",
		Fields: []query.FieldMetadata{keyMetadata, valueMetadata},
	}
	return bindingValue{&jsonString, jsonFormatStr, &schema}, nil
}

func toNullableInt64(val any) (int64, bool) {
	switch v := val.(type) {
	case sql.NullByte:
		return int64(v.Byte), v.Valid
	case sql.NullInt16:
		return int64(v.Int16), v.Valid
	case sql.NullInt32:
		return int64(v.Int32), v.Valid
	case sql.NullInt64:
		return v.Int64, v.Valid
	}
	// should never happen, the list above is exhaustive
	panic("Only byte, int16, int32 or int64 are supported")
}

func toNullableTime(val any) (time.Time, bool, error) {
	switch v := val.(type) {
	case time.Time:
		return v, true, nil
	case sql.NullTime:
		return v.Time, v.Valid, nil
	}
	return time.Now(), false, fmt.Errorf("cannot use %T as time", val)
}

func stringOrIntToString(v reflect.Value) string {
	if v.CanInt() {
		return strconv.Itoa(int(v.Int()))
	}
	return v.String()
}

func goTypeToFieldMetadata(typ reflect.Type, tsmode types.SnowflakeType, params *syncParams) (query.FieldMetadata, error) {
	if tsmode == types.BinaryType {
		return query.FieldMetadata{
			Type:     "BINARY",
			Nullable: true,
		}, nil
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.String:
		return query.FieldMetadata{
			Type:     "TEXT",
			Nullable: true,
		}, nil
	case reflect.Bool:
		return query.FieldMetadata{
			Type:     "BOOLEAN",
			Nullable: true,
		}, nil
	case reflect.Int, reflect.Int8, reflect.Uint8, reflect.Int16, reflect.Int32, reflect.Int64:
		return query.FieldMetadata{
			Type:      "FIXED",
			Precision: numberDefaultPrecision,
			Nullable:  true,
		}, nil
	case reflect.Float32, reflect.Float64:
		return query.FieldMetadata{
			Type:     "REAL",
			Nullable: true,
		}, nil
	case reflect.Struct:
		if typ.AssignableTo(reflect.TypeFor[sql.NullString]()) {
			return query.FieldMetadata{
				Type:     "TEXT",
				Nullable: true,
			}, nil
		} else if typ.AssignableTo(reflect.TypeFor[sql.NullBool]()) {
			return query.FieldMetadata{
				Type:     "BOOLEAN",
				Nullable: true,
			}, nil
		} else if typ.AssignableTo(reflect.TypeFor[sql.NullByte]()) || typ.AssignableTo(reflect.TypeFor[sql.NullInt16]()) || typ.AssignableTo(reflect.TypeFor[sql.NullInt32]()) || typ.AssignableTo(reflect.TypeFor[sql.NullInt64]()) {
			return query.FieldMetadata{
				Type:      "FIXED",
				Precision: numberDefaultPrecision,
				Nullable:  true,
			}, nil
		} else if typ.AssignableTo(reflect.TypeFor[sql.NullFloat64]()) {
			return query.FieldMetadata{
				Type:     "REAL",
				Nullable: true,
			}, nil
		} else if tsmode == types.DateType {
			return query.FieldMetadata{
				Type:     "DATE",
				Nullable: true,
			}, nil
		} else if tsmode == types.TimeType {
			return query.FieldMetadata{
				Type:     "TIME",
				Nullable: true,
			}, nil
		} else if tsmode == types.TimestampTzType {
			return query.FieldMetadata{
				Type:     "TIMESTAMP_TZ",
				Nullable: true,
			}, nil
		} else if tsmode == types.TimestampNtzType {
			return query.FieldMetadata{
				Type:     "TIMESTAMP_NTZ",
				Nullable: true,
			}, nil
		} else if tsmode == types.TimestampLtzType {
			return query.FieldMetadata{
				Type:     "TIMESTAMP_LTZ",
				Nullable: true,
			}, nil
		} else if typ.AssignableTo(structuredObjectWriterType) || tsmode == types.NilObjectType {
			sowc, err := buildSowcFromType(params, typ)
			if err != nil {
				return query.FieldMetadata{}, err
			}
			return query.FieldMetadata{
				Type:     "OBJECT",
				Nullable: true,
				Fields:   sowc.toFields(),
			}, nil
		} else if tsmode == types.NilArrayType || tsmode == types.NilMapType {
			sowc, err := buildSowcFromType(params, typ)
			if err != nil {
				return query.FieldMetadata{}, err
			}
			return query.FieldMetadata{
				Type:     "OBJECT",
				Nullable: true,
				Fields:   sowc.toFields(),
			}, nil
		}
	case reflect.Slice:
		metadata, err := goTypeToFieldMetadata(typ.Elem(), tsmode, params)
		if err != nil {
			return query.FieldMetadata{}, err
		}
		return query.FieldMetadata{
			Type:     "ARRAY",
			Nullable: true,
			Fields:   []query.FieldMetadata{metadata},
		}, nil
	case reflect.Map:
		keyMetadata, err := goTypeToFieldMetadata(typ.Key(), tsmode, params)
		if err != nil {
			return query.FieldMetadata{}, err
		}
		valueMetadata, err := goTypeToFieldMetadata(typ.Elem(), tsmode, params)
		if err != nil {
			return query.FieldMetadata{}, err
		}
		return query.FieldMetadata{
			Type:     "MAP",
			Nullable: true,
			Fields:   []query.FieldMetadata{keyMetadata, valueMetadata},
		}, nil
	}
	return query.FieldMetadata{}, fmt.Errorf("cannot build field metadata for %v (mode %v)", typ.Kind().String(), tsmode.String())
}

func isSliceOfSlices(v any) bool {
	typ := reflect.TypeOf(v)
	return typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.Slice
}

func isArrayOfStructs(v any) bool {
	return reflect.TypeOf(v).Elem().Kind() == reflect.Struct || (reflect.TypeOf(v).Elem().Kind() == reflect.Pointer && reflect.TypeOf(v).Elem().Elem().Kind() == reflect.Struct)
}

func structValueToString(v driver.Value, tsmode types.SnowflakeType, params *syncParams) (bindingValue, error) {
	switch typedVal := v.(type) {
	case time.Time:
		return timeTypeValueToString(typedVal, tsmode)
	case sql.NullTime:
		if !typedVal.Valid {
			return bindingValue{nil, "", nil}, nil
		}
		return timeTypeValueToString(typedVal.Time, tsmode)
	case sql.NullBool:
		if !typedVal.Valid {
			return bindingValue{nil, "", nil}, nil
		}
		s := strconv.FormatBool(typedVal.Bool)
		return bindingValue{&s, "", nil}, nil
	case sql.NullInt64:
		if !typedVal.Valid {
			return bindingValue{nil, "", nil}, nil
		}
		s := strconv.FormatInt(typedVal.Int64, 10)
		return bindingValue{&s, "", nil}, nil
	case sql.NullFloat64:
		if !typedVal.Valid {
			return bindingValue{nil, "", nil}, nil
		}
		s := strconv.FormatFloat(typedVal.Float64, 'g', -1, 32)
		return bindingValue{&s, "", nil}, nil
	case sql.NullString:
		fmt := ""
		if isJSONFormatType(tsmode) {
			fmt = jsonFormatStr
		}
		if !typedVal.Valid {
			return bindingValue{nil, fmt, nil}, nil
		}
		return bindingValue{&typedVal.String, fmt, nil}, nil
	}
	if sow, ok := v.(StructuredObjectWriter); ok {
		sowc := &structuredObjectWriterContext{}
		sowc.init(params)
		err := sow.Write(sowc)
		if err != nil {
			return bindingValue{nil, "", nil}, err
		}
		jsonBytes, err := json.Marshal(sowc.values)
		if err != nil {
			return bindingValue{nil, "", nil}, err
		}
		jsonString := string(jsonBytes)
		schema := bindingSchema{
			Typ:      "object",
			Nullable: true,
			Fields:   sowc.toFields(),
		}
		return bindingValue{&jsonString, jsonFormatStr, &schema}, nil
	} else if typ, ok := v.(reflect.Type); ok && tsmode == types.NilArrayType {
		metadata, err := goTypeToFieldMetadata(typ, tsmode, params)
		if err != nil {
			return bindingValue{}, err
		}
		schema := bindingSchema{
			Typ:      "ARRAY",
			Nullable: true,
			Fields: []query.FieldMetadata{
				metadata,
			},
		}
		return bindingValue{nil, jsonFormatStr, &schema}, nil
	} else if t, ok := v.(NilMapTypes); ok && tsmode == types.NilMapType {
		keyMetadata, err := goTypeToFieldMetadata(t.Key, tsmode, params)
		if err != nil {
			return bindingValue{}, err
		}
		valueMetadata, err := goTypeToFieldMetadata(t.Value, tsmode, params)
		if err != nil {
			return bindingValue{}, err
		}
		schema := bindingSchema{
			Typ:      "map",
			Nullable: true,
			Fields:   []query.FieldMetadata{keyMetadata, valueMetadata},
		}
		return bindingValue{nil, jsonFormatStr, &schema}, nil
	} else if typ, ok := v.(reflect.Type); ok && tsmode == types.NilObjectType {
		metadata, err := goTypeToFieldMetadata(typ, tsmode, params)
		if err != nil {
			return bindingValue{}, err
		}
		schema := bindingSchema{
			Typ:      "object",
			Nullable: true,
			Fields:   metadata.Fields,
		}
		return bindingValue{nil, jsonFormatStr, &schema}, nil
	}
	return bindingValue{}, fmt.Errorf("unknown binding for type %T and mode %v", v, tsmode)
}

func timeTypeValueToString(tm time.Time, tsmode types.SnowflakeType) (bindingValue, error) {
	switch tsmode {
	case types.DateType:
		_, offset := tm.Zone()
		tm = tm.Add(time.Second * time.Duration(offset))
		s := strconv.FormatInt(tm.Unix()*1000, 10)
		return bindingValue{&s, "", nil}, nil
	case types.TimeType:
		s := strconv.Itoa((tm.Hour()*3600+tm.Minute()*60+tm.Second())*1e9 + tm.Nanosecond())
		return bindingValue{&s, "", nil}, nil
	case types.TimestampNtzType, types.TimestampLtzType, types.TimestampTzType:
		s, err := convertTimeToTimeStamp(tm, tsmode)
		if err != nil {
			return bindingValue{nil, "", nil}, err
		}
		return bindingValue{&s, "", nil}, nil
	}
	return bindingValue{nil, "", nil}, fmt.Errorf("unsupported time type: %v", tsmode)
}

// extractTimestamp extracts the internal timestamp data to epoch time in seconds and milliseconds
func extractTimestamp(srcValue *string) (sec int64, nsec int64, err error) {
	if logger.GetLogLevelInt() <= sflog.LevelDebug {
		logger.Debugf("SRC: %v", srcValue)
	}
	var i int
	for i = 0; i < len(*srcValue); i++ {
		if (*srcValue)[i] == '.' {
			sec, err = strconv.ParseInt((*srcValue)[0:i], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			break
		}
	}
	if i == len(*srcValue) {
		// no fraction
		sec, err = strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		nsec = 0
	} else {
		s := (*srcValue)[i+1:]
		nsec, err = strconv.ParseInt(s+strings.Repeat("0", 9-len(s)), 10, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	if logger.GetLogLevelInt() <= sflog.LevelDebug {
		logger.Debugf("sec: %v, nsec: %v", sec, nsec)
	}
	return sec, nsec, nil
}

// stringToValue converts a pointer of string data to an arbitrary golang variable
// This is mainly used in fetching data.
func stringToValue(ctx context.Context, dest *driver.Value, srcColumnMeta query.ExecResponseRowType, srcValue *string, loc *time.Location, params *syncParams) error {
	if srcValue == nil {
		if logger.GetLogLevelInt() <= sflog.LevelDebug {
			logger.Debugf("snowflake data type: %v, raw value: nil", srcColumnMeta.Type)
		}
		*dest = nil
		return nil
	}
	structuredTypesEnabled := structuredTypesEnabled(ctx)

	// Truncate large strings before logging to avoid secret masking performance issues.
	// The truncation and the Debugf arguments run only when debug logging is on.
	if logger.GetLogLevelInt() <= sflog.LevelDebug {
		valueForLogging := *srcValue
		if len(valueForLogging) > 1024 {
			valueForLogging = valueForLogging[:1024] + fmt.Sprintf("... (%d bytes total)", len(*srcValue))
		}
		logger.Debugf("snowflake data type: %v, raw value: %v", srcColumnMeta.Type, valueForLogging)
	}
	switch srcColumnMeta.Type {
	case "object":
		if len(srcColumnMeta.Fields) == 0 || !structuredTypesEnabled {
			// semistructured type without schema
			*dest = *srcValue
			return nil
		}
		m := make(map[string]any)
		decoder := decoderWithNumbersAsStrings(srcValue)
		if err := decoder.Decode(&m); err != nil {
			return err
		}
		v, err := buildStructuredTypeRecursive(ctx, m, srcColumnMeta.Fields, params)
		if err != nil {
			return err
		}
		*dest = v
		return nil
	case "text", "real", "variant":
		*dest = *srcValue
		return nil
	case "fixed":
		if higherPrecisionEnabled(ctx) {
			if srcColumnMeta.Scale == 0 {
				if srcColumnMeta.Precision >= 19 {
					bigInt := big.NewInt(0)
					bigInt.SetString(*srcValue, 10)
					*dest = *bigInt
					return nil
				}
				*dest = *srcValue
				return nil
			}
			bigFloat, _, err := big.ParseFloat(*srcValue, 10, numberMaxPrecisionInBits, big.AwayFromZero)
			if err != nil {
				return err
			}
			*dest = *bigFloat
			return nil
		}
		*dest = *srcValue
		return nil
	case "decfloat":
		if !decfloatMappingEnabled(ctx) {
			*dest = *srcValue
			return nil
		}
		bf := new(big.Float).SetPrec(127)
		if _, ok := bf.SetString(*srcValue); !ok {
			return fmt.Errorf("cannot convert %v to %T", *srcValue, bf)
		}
		if higherPrecisionEnabled(ctx) {
			*dest = *bf
		} else {
			*dest, _ = bf.Float64()
		}
		return nil
	case "date":
		v, err := strconv.ParseInt(*srcValue, 10, 64)
		if err != nil {
			return err
		}
		*dest = time.Unix(v*86400, 0).UTC()
		return nil
	case "time":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		t0 := time.Time{}
		*dest = t0.Add(time.Duration(sec*1e9 + nsec))
		return nil
	case "timestamp_ntz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		*dest = time.Unix(sec, nsec).UTC()
		return nil
	case "timestamp_ltz":
		sec, nsec, err := extractTimestamp(srcValue)
		if err != nil {
			return err
		}
		if loc == nil {
			loc = time.Now().Location()
		}
		*dest = time.Unix(sec, nsec).In(loc)
		return nil
	case "timestamp_tz":
		if logger.GetLogLevelInt() <= sflog.LevelDebug {
			logger.Debugf("tz: %v", *srcValue)
		}

		tm := strings.Split(*srcValue, " ")
		if len(tm) != 2 {
			return &SnowflakeError{
				Number:   ErrInvalidTimestampTz,
				SQLState: SQLStateInvalidDataTimeFormat,
				Message:  fmt.Sprintf("invalid TIMESTAMP_TZ data. The value doesn't consist of two numeric values separated by a space: %v", *srcValue),
			}
		}
		sec, nsec, err := extractTimestamp(&tm[0])
		if err != nil {
			return err
		}
		offset, err := strconv.ParseInt(tm[1], 10, 64)
		if err != nil {
			return &SnowflakeError{
				Number:   ErrInvalidTimestampTz,
				SQLState: SQLStateInvalidDataTimeFormat,
				Message:  fmt.Sprintf("invalid TIMESTAMP_TZ data. The offset value is not integer: %v", tm[1]),
			}
		}
		loc := Location(int(offset) - 1440)
		tt := time.Unix(sec, nsec)
		*dest = tt.In(loc)
		return nil
	case "binary":
		b, err := hex.DecodeString(*srcValue)
		if err != nil {
			return &SnowflakeError{
				Number:   ErrInvalidBinaryHexForm,
				SQLState: SQLStateNumericValueOutOfRange,
				Message:  err.Error(),
			}
		}
		*dest = b
		return nil
	case "array":
		if len(srcColumnMeta.Fields) == 0 || !structuredTypesEnabled {
			*dest = *srcValue
			return nil
		}
		if len(srcColumnMeta.Fields) > 1 {
			return errors.New("got more than one field for array")
		}
		var arr []any
		decoder := decoderWithNumbersAsStrings(srcValue)
		if err := decoder.Decode(&arr); err != nil {
			return err
		}
		v, err := buildStructuredArray(ctx, srcColumnMeta.Fields[0], arr, params)
		if err != nil {
			return err
		}
		*dest = v
		return nil
	case "map":
		var err error
		*dest, err = jsonToMap(ctx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], *srcValue, params)
		return err
	}
	*dest = *srcValue
	return nil
}

func jsonToMap(ctx context.Context, keyMetadata, valueMetadata query.FieldMetadata, srcValue string, params *syncParams) (snowflakeValue, error) {
	structuredTypesEnabled := structuredTypesEnabled(ctx)
	if !structuredTypesEnabled {
		return srcValue, nil
	}
	switch keyMetadata.Type {
	case "text":
		var m map[string]any
		decoder := decoderWithNumbersAsStrings(&srcValue)
		err := decoder.Decode(&m)
		if err != nil {
			return nil, err
		}
		// returning snowflakeValue of complex types does not work with generics
		if valueMetadata.Type == "object" {
			res := make(map[string]*structuredType)
			for k, v := range m {
				if v == nil || reflect.ValueOf(v).IsNil() {
					res[k] = nil
				} else {
					res[k] = buildStructuredTypeFromMap(v.(map[string]any), valueMetadata.Fields, params)
				}
			}
			return res, nil
		}
		return jsonToMapWithKeyType[string](ctx, valueMetadata, m, params)
	case "fixed":
		var m map[int64]any
		decoder := decoderWithNumbersAsStrings(&srcValue)
		err := decoder.Decode(&m)
		if err != nil {
			return nil, err
		}
		if valueMetadata.Type == "object" {
			res := make(map[int64]*structuredType)
			for k, v := range m {
				res[k] = buildStructuredTypeFromMap(v.(map[string]any), valueMetadata.Fields, params)
			}
			return res, nil
		}
		return jsonToMapWithKeyType[int64](ctx, valueMetadata, m, params)
	default:
		return nil, fmt.Errorf("unsupported map key type: %v", keyMetadata.Type)
	}
}

func jsonToMapWithKeyType[K comparable](ctx context.Context, valueMetadata query.FieldMetadata, m map[K]any, params *syncParams) (snowflakeValue, error) {
	mapValuesNullableEnabled := embeddedValuesNullableEnabled(ctx)
	switch valueMetadata.Type {
	case "text":
		return buildMapValues[K, sql.NullString, string](mapValuesNullableEnabled, m, func(v any) (string, error) {
			return v.(string), nil
		}, func(v any) (sql.NullString, error) {
			return sql.NullString{Valid: v != nil, String: ifNotNullOrDefault(v, "")}, nil
		}, false)
	case "boolean":
		return buildMapValues[K, sql.NullBool, bool](mapValuesNullableEnabled, m, func(v any) (bool, error) {
			return v.(bool), nil
		}, func(v any) (sql.NullBool, error) {
			return sql.NullBool{Valid: v != nil, Bool: ifNotNullOrDefault(v, false)}, nil
		}, false)
	case "fixed":
		if valueMetadata.Scale == 0 {
			return buildMapValues[K, sql.NullInt64, int64](mapValuesNullableEnabled, m, func(v any) (int64, error) {
				return strconv.ParseInt(string(v.(json.Number)), 10, 64)
			}, func(v any) (sql.NullInt64, error) {
				if v != nil {
					i64, err := strconv.ParseInt(string(v.(json.Number)), 10, 64)
					if err != nil {
						return sql.NullInt64{}, err
					}
					return sql.NullInt64{Valid: true, Int64: i64}, nil
				}
				return sql.NullInt64{Valid: false}, nil
			}, false)
		}
		return buildMapValues[K, sql.NullFloat64, float64](mapValuesNullableEnabled, m, func(v any) (float64, error) {
			return strconv.ParseFloat(string(v.(json.Number)), 64)
		}, func(v any) (sql.NullFloat64, error) {
			if v != nil {
				f64, err := strconv.ParseFloat(string(v.(json.Number)), 64)
				if err != nil {
					return sql.NullFloat64{}, err
				}
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			}
			return sql.NullFloat64{Valid: false}, nil
		}, false)
	case "real":
		return buildMapValues[K, sql.NullFloat64, float64](mapValuesNullableEnabled, m, func(v any) (float64, error) {
			return strconv.ParseFloat(string(v.(json.Number)), 64)
		}, func(v any) (sql.NullFloat64, error) {
			if v != nil {
				f64, err := strconv.ParseFloat(string(v.(json.Number)), 64)
				if err != nil {
					return sql.NullFloat64{}, err
				}
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			}
			return sql.NullFloat64{Valid: false}, nil
		}, false)
	case "binary":
		return buildMapValues[K, []byte, []byte](mapValuesNullableEnabled, m, func(v any) ([]byte, error) {
			if v == nil {
				return nil, nil
			}
			return hex.DecodeString(v.(string))
		}, func(v any) ([]byte, error) {
			if v == nil {
				return nil, nil
			}
			return hex.DecodeString(v.(string))
		}, true)
	case "date", "time", "timestamp_tz", "timestamp_ltz", "timestamp_ntz":
		return buildMapValues[K, sql.NullTime, time.Time](mapValuesNullableEnabled, m, func(v any) (time.Time, error) {
			sfFormat, err := dateTimeOutputFormatByType(valueMetadata.Type, params)
			if err != nil {
				return time.Time{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return time.Time{}, err
			}
			return time.Parse(goFormat, v.(string))
		}, func(v any) (sql.NullTime, error) {
			if v == nil {
				return sql.NullTime{Valid: false}, nil
			}
			sfFormat, err := dateTimeOutputFormatByType(valueMetadata.Type, params)
			if err != nil {
				return sql.NullTime{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return sql.NullTime{}, err
			}
			time, err := time.Parse(goFormat, v.(string))
			if err != nil {
				return sql.NullTime{}, err
			}
			return sql.NullTime{Valid: true, Time: time}, nil
		}, false)
	case "array":
		arrayMetadata := valueMetadata.Fields[0]
		switch arrayMetadata.Type {
		case "text":
			return buildArrayFromMap[K, string](ctx, arrayMetadata, m, params)
		case "fixed":
			if arrayMetadata.Scale == 0 {
				return buildArrayFromMap[K, int64](ctx, arrayMetadata, m, params)
			}
			return buildArrayFromMap[K, float64](ctx, arrayMetadata, m, params)
		case "real":
			return buildArrayFromMap[K, float64](ctx, arrayMetadata, m, params)
		case "binary":
			return buildArrayFromMap[K, []byte](ctx, arrayMetadata, m, params)
		case "boolean":
			return buildArrayFromMap[K, bool](ctx, arrayMetadata, m, params)
		case "date", "time", "timestamp_ltz", "timestamp_tz", "timestamp_ntz":
			return buildArrayFromMap[K, time.Time](ctx, arrayMetadata, m, params)
		}
	}
	return nil, fmt.Errorf("unsupported map value type: %v", valueMetadata.Type)
}

func buildArrayFromMap[K comparable, V any](ctx context.Context, valueMetadata query.FieldMetadata, m map[K]any, params *syncParams) (snowflakeValue, error) {
	res := make(map[K][]V)
	for k, v := range m {
		if v == nil {
			res[k] = nil
		} else {
			structuredArray, err := buildStructuredArray(ctx, valueMetadata, v.([]any), params)
			if err != nil {
				return nil, err
			}
			res[k] = structuredArray.([]V)
		}
	}
	return res, nil
}

func buildStructuredTypeFromMap(values map[string]any, fieldMetadata []query.FieldMetadata, params *syncParams) *structuredType {
	return &structuredType{
		values:        values,
		params:        params,
		fieldMetadata: fieldMetadata,
	}
}

func ifNotNullOrDefault[T any](t any, def T) T {
	if t == nil {
		return def
	}
	return t.(T)
}

func buildMapValues[K comparable, Vnullable any, VnotNullable any](mapValuesNullableEnabled bool, m map[K]any, buildNotNullable func(v any) (VnotNullable, error), buildNullable func(v any) (Vnullable, error), nullableByDefault bool) (snowflakeValue, error) {
	var err error
	if mapValuesNullableEnabled {
		result := make(map[K]Vnullable, len(m))
		for k, v := range m {
			if result[k], err = buildNullable(v); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	result := make(map[K]VnotNullable, len(m))
	for k, v := range m {
		if v == nil && !nullableByDefault {
			return nil, errors2.ErrNullValueInMapError()
		}
		if result[k], err = buildNotNullable(v); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func buildStructuredArray(ctx context.Context, fieldMetadata query.FieldMetadata, srcValue []any, params *syncParams) (any, error) {
	switch fieldMetadata.Type {
	case "text":
		return copyArrayAndConvert[string](srcValue, func(input any) (string, error) {
			return input.(string), nil
		})
	case "fixed":
		if fieldMetadata.Scale == 0 {
			return copyArrayAndConvert[int64](srcValue, func(input any) (int64, error) {
				return strconv.ParseInt(string(input.(json.Number)), 10, 64)
			})
		}
		return copyArrayAndConvert[float64](srcValue, func(input any) (float64, error) {
			return strconv.ParseFloat(string(input.(json.Number)), 64)
		})
	case "real":
		return copyArrayAndConvert[float64](srcValue, func(input any) (float64, error) {
			return strconv.ParseFloat(string(input.(json.Number)), 64)
		})
	case "time", "date", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
		return copyArrayAndConvert[time.Time](srcValue, func(input any) (time.Time, error) {
			sfFormat, err := dateTimeOutputFormatByType(fieldMetadata.Type, params)
			if err != nil {
				return time.Time{}, err
			}
			goFormat, err := snowflakeFormatToGoFormat(sfFormat)
			if err != nil {
				return time.Time{}, err
			}
			return time.Parse(goFormat, input.(string))
		})
	case "boolean":
		return copyArrayAndConvert[bool](srcValue, func(input any) (bool, error) {
			return input.(bool), nil
		})
	case "binary":
		return copyArrayAndConvert[[]byte](srcValue, func(input any) ([]byte, error) {
			return hex.DecodeString(input.(string))
		})
	case "object":
		return copyArrayAndConvert[*structuredType](srcValue, func(input any) (*structuredType, error) {
			return buildStructuredTypeRecursive(ctx, input.(map[string]any), fieldMetadata.Fields, params)
		})
	case "array":
		switch fieldMetadata.Fields[0].Type {
		case "text":
			return buildStructuredArrayRecursive[string](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "fixed":
			if fieldMetadata.Fields[0].Scale == 0 {
				return buildStructuredArrayRecursive[int64](ctx, fieldMetadata.Fields[0], srcValue, params)
			}
			return buildStructuredArrayRecursive[float64](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "real":
			return buildStructuredArrayRecursive[float64](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "boolean":
			return buildStructuredArrayRecursive[bool](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "binary":
			return buildStructuredArrayRecursive[[]byte](ctx, fieldMetadata.Fields[0], srcValue, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			return buildStructuredArrayRecursive[time.Time](ctx, fieldMetadata.Fields[0], srcValue, params)
		}
	}
	return srcValue, nil
}

func buildStructuredArrayRecursive[T any](ctx context.Context, fieldMetadata query.FieldMetadata, srcValue []any, params *syncParams) ([][]T, error) {
	arr := make([][]T, len(srcValue))
	for i, v := range srcValue {
		structuredArray, err := buildStructuredArray(ctx, fieldMetadata, v.([]any), params)
		if err != nil {
			return nil, err
		}
		arr[i] = structuredArray.([]T)
	}
	return arr, nil
}

func copyArrayAndConvert[T any](input []any, convertFunc func(input any) (T, error)) ([]T, error) {
	var err error
	output := make([]T, len(input))
	for i, s := range input {
		if output[i], err = convertFunc(s); err != nil {
			return nil, err
		}
	}
	return output, nil
}

func buildStructuredTypeRecursive(ctx context.Context, m map[string]any, fields []query.FieldMetadata, params *syncParams) (*structuredType, error) {
	var err error
	for _, fm := range fields {
		if fm.Type == "array" && m[fm.Name] != nil {
			if m[fm.Name], err = buildStructuredArray(ctx, fm.Fields[0], m[fm.Name].([]any), params); err != nil {
				return nil, err
			}
		} else if fm.Type == "map" && m[fm.Name] != nil {
			if m[fm.Name], err = jsonToMapWithKeyType(ctx, fm.Fields[1], m[fm.Name].(map[string]any), params); err != nil {
				return nil, err
			}
		} else if fm.Type == "object" && m[fm.Name] != nil {
			if m[fm.Name], err = buildStructuredTypeRecursive(ctx, m[fm.Name].(map[string]any), fm.Fields, params); err != nil {
				return nil, err
			}
		}
	}
	return &structuredType{
		values:        m,
		fieldMetadata: fields,
		params:        params,
	}, nil
}

var decimalShift = new(big.Int).Exp(big.NewInt(2), big.NewInt(64), nil)

func intToBigFloat(val int64, scale int64) *big.Float {
	f := new(big.Float).SetInt64(val)
	s := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	return new(big.Float).Quo(f, s)
}

func decimalToBigInt(num decimal128.Num) *big.Int {
	high := new(big.Int).SetInt64(num.HighBits())
	low := new(big.Int).SetUint64(num.LowBits())
	return new(big.Int).Add(new(big.Int).Mul(high, decimalShift), low)
}

func decimalToBigFloat(num decimal128.Num, scale int64) *big.Float {
	f := new(big.Float).SetInt(decimalToBigInt(num))
	s := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	return new(big.Float).Quo(f, s)
}

func arrowSnowflakeTimestampToTime(
	column arrow.Array,
	sfType types.SnowflakeType,
	scale int,
	recIdx int,
	loc *time.Location) *time.Time {

	if column.IsNull(recIdx) {
		return nil
	}
	var ret time.Time
	switch sfType {
	case types.TimestampNtzType:
		if column.DataType().ID() == arrow.STRUCT {
			structData := column.(*array.Struct)
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).UTC()
		} else {
			intData := column.(*array.Int64)
			value := intData.Value(recIdx)
			epoch := extractEpoch(value, scale)
			fraction := extractFraction(value, scale)
			ret = time.Unix(epoch, fraction).UTC()
		}
	case types.TimestampLtzType:
		if column.DataType().ID() == arrow.STRUCT {
			structData := column.(*array.Struct)
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).In(loc)
		} else {
			intData := column.(*array.Int64)
			value := intData.Value(recIdx)
			epoch := extractEpoch(value, scale)
			fraction := extractFraction(value, scale)
			ret = time.Unix(epoch, fraction).In(loc)
		}
	case types.TimestampTzType:
		structData := column.(*array.Struct)
		if structData.NumField() == 2 {
			value := structData.Field(0).(*array.Int64).Int64Values()
			timezone := structData.Field(1).(*array.Int32).Int32Values()
			epoch := extractEpoch(value[recIdx], scale)
			fraction := extractFraction(value[recIdx], scale)
			locTz := Location(int(timezone[recIdx]) - 1440)
			ret = time.Unix(epoch, fraction).In(locTz)
		} else {
			epoch := structData.Field(0).(*array.Int64).Int64Values()
			fraction := structData.Field(1).(*array.Int32).Int32Values()
			timezone := structData.Field(2).(*array.Int32).Int32Values()
			locTz := Location(int(timezone[recIdx]) - 1440)
			ret = time.Unix(epoch[recIdx], int64(fraction[recIdx])).In(locTz)
		}
	}
	return &ret
}

func extractEpoch(value int64, scale int) int64 {
	return value / int64(math.Pow10(scale))
}

func extractFraction(value int64, scale int) int64 {
	return (value % int64(math.Pow10(scale))) * int64(math.Pow10(9-scale))
}

// Arrow Interface (Column) converter. This is called when Arrow chunks are
// downloaded to convert to the corresponding row type.
func arrowToValues(
	ctx context.Context,
	destcol []snowflakeValue,
	srcColumnMeta query.ExecResponseRowType,
	srcValue arrow.Array,
	loc *time.Location,
	higherPrecision bool,
	params *syncParams) error {

	if len(destcol) != srcValue.Len() {
		return fmt.Errorf("array interface length mismatch")
	}
	if logger.GetLogLevelInt() <= sflog.LevelDebug {
		logger.Debugf("snowflake data type: %v, arrow data type: %v", srcColumnMeta.Type, srcValue.DataType())
	}

	var err error
	snowflakeType := types.GetSnowflakeType(srcColumnMeta.Type)
	for i := range destcol {
		if destcol[i], err = arrowToValue(ctx, i, srcColumnMeta.ToFieldMetadata(), srcValue, loc, higherPrecision, params, snowflakeType); err != nil {
			return err
		}
	}
	return nil
}

// unexpectedArrowStructured reports a native Arrow column that is not JSON text.
// kind is ARRAY or OBJECT. expected is the native Arrow type required when the
// column has field metadata and structured types are enabled.
func unexpectedArrowStructured(src arrow.Array, hasSchema, enabled bool, kind, expected string) error {
	got := src.DataType().String()
	if !hasSchema {
		return fmt.Errorf("semi-structured %s arrived as %s, expected a JSON string", kind, got)
	}
	if !enabled {
		return errNativeArrowWithoutProperContext
	}
	return fmt.Errorf("structured %s arrived as %s, expected %s", kind, got, expected)
}

func arrowToValue(ctx context.Context, rowIdx int, srcColumnMeta query.FieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params *syncParams, snowflakeType types.SnowflakeType) (snowflakeValue, error) {
	structuredTypesEnabled := structuredTypesEnabled(ctx)
	switch snowflakeType {
	case types.FixedType:
		// Snowflake data types that are fixed-point numbers will fall into this category
		// e.g. NUMBER, DECIMAL/NUMERIC, INT/INTEGER
		switch numericValue := srcValue.(type) {
		case *array.Decimal128:
			return arrowDecimal128ToValue(numericValue, rowIdx, higherPrecision, srcColumnMeta), nil
		case *array.Int64:
			return arrowInt64ToValue(numericValue, rowIdx, higherPrecision, srcColumnMeta), nil
		case *array.Int32:
			return arrowInt32ToValue(numericValue, rowIdx, higherPrecision, srcColumnMeta), nil
		case *array.Int16:
			return arrowInt16ToValue(numericValue, rowIdx, higherPrecision, srcColumnMeta), nil
		case *array.Int8:
			return arrowInt8ToValue(numericValue, rowIdx, higherPrecision, srcColumnMeta), nil
		}
		return nil, fmt.Errorf("unsupported data type")
	case types.RealType:
		// Snowflake data types that are floating-point numbers will fall in this category
		// e.g. FLOAT/REAL/DOUBLE
		return arrowRealToValue(srcValue.(*array.Float64), rowIdx), nil
	case types.DecfloatType:
		return arrowDecFloatToValue(ctx, srcValue.(*array.Struct), rowIdx)
	case types.BooleanType:
		return arrowBoolToValue(srcValue.(*array.Boolean), rowIdx), nil
	case types.TextType, types.VariantType:
		strings := srcValue.(*array.String)
		if !srcValue.IsNull(rowIdx) {
			return strings.Value(rowIdx), nil
		}
		return nil, nil
	case types.ArrayType:
		if srcValue.IsNull(rowIdx) {
			return nil, nil
		}
		// JSON text is the semi-structured representation. With field metadata present,
		// structured types disabled keeps that string so existing callers still scan a string.
		if stringColumn, ok := srcValue.(*array.String); ok {
			text := stringColumn.Value(rowIdx)
			if len(srcColumnMeta.Fields) == 0 || !structuredTypesEnabled {
				return text, nil
			}
			var arr []any
			decoder := decoderWithNumbersAsStrings(&text)
			if err := decoder.Decode(&arr); err != nil {
				return nil, err
			}
			return buildStructuredArray(ctx, srcColumnMeta.Fields[0], arr, params)
		}
		if len(srcColumnMeta.Fields) == 0 || !structuredTypesEnabled {
			return nil, unexpectedArrowStructured(srcValue, len(srcColumnMeta.Fields) > 0, structuredTypesEnabled, "ARRAY", "an Arrow list")
		}
		return buildListFromNativeArrow(ctx, rowIdx, srcColumnMeta.Fields[0], srcValue, loc, higherPrecision, params)
	case types.ObjectType:
		if srcValue.IsNull(rowIdx) {
			return nil, nil
		}
		if stringColumn, ok := srcValue.(*array.String); ok {
			text := stringColumn.Value(rowIdx)
			if len(srcColumnMeta.Fields) == 0 || !structuredTypesEnabled {
				return text, nil
			}
			m := make(map[string]any)
			decoder := decoderWithNumbersAsStrings(&text)
			if err := decoder.Decode(&m); err != nil {
				return nil, err
			}
			return buildStructuredTypeRecursive(ctx, m, srcColumnMeta.Fields, params)
		}
		if structs, ok := srcValue.(*array.Struct); ok && len(srcColumnMeta.Fields) > 0 && structuredTypesEnabled {
			return arrowToStructuredType(ctx, structs, srcColumnMeta.Fields, loc, rowIdx, higherPrecision, params)
		}
		return nil, unexpectedArrowStructured(srcValue, len(srcColumnMeta.Fields) > 0, structuredTypesEnabled, "OBJECT", "an Arrow struct")
	case types.MapType:
		if srcValue.IsNull(rowIdx) {
			return nil, nil
		}
		strings, ok := srcValue.(*array.String)
		if ok {
			// structured map as json
			if !srcValue.IsNull(rowIdx) {
				return jsonToMap(ctx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], strings.Value(rowIdx), params)
			}
		} else {
			// structured map as native arrow
			if !structuredTypesEnabled {
				return nil, errNativeArrowWithoutProperContext
			}
			return buildMapFromNativeArrow(ctx, rowIdx, srcColumnMeta.Fields[0], srcColumnMeta.Fields[1], srcValue, loc, higherPrecision, params)
		}
	case types.BinaryType:
		return arrowBinaryToValue(srcValue.(*array.Binary), rowIdx), nil
	case types.DateType:
		return arrowDateToValue(srcValue.(*array.Date32), rowIdx), nil
	case types.TimeType:
		return arrowTimeToValue(srcValue, rowIdx, int(srcColumnMeta.Scale)), nil
	case types.TimestampNtzType, types.TimestampLtzType, types.TimestampTzType:
		v := arrowSnowflakeTimestampToTime(srcValue, snowflakeType, int(srcColumnMeta.Scale), rowIdx, loc)
		if v != nil {
			return *v, nil
		}
		return nil, nil
	}

	return nil, fmt.Errorf("unsupported data type")
}

func buildMapFromNativeArrow(ctx context.Context, rowIdx int, keyMetadata, valueMetadata query.FieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params *syncParams) (snowflakeValue, error) {
	arrowMap := srcValue.(*array.Map)
	if arrowMap.IsNull(rowIdx) {
		return nil, nil
	}
	keys := arrowMap.Keys()
	items := arrowMap.Items()
	offsets := arrowMap.Offsets()
	switch keyMetadata.Type {
	case "text":
		keyFunc := func(j int) (string, error) {
			return keys.(*array.String).Value(j), nil
		}
		return buildStructuredMapFromArrow(ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
	case "fixed":
		keyFunc := func(j int) (int64, error) {
			k, err := extractInt64(keys, int(j))
			if err != nil {
				return 0, err
			}
			return k, nil
		}
		return buildStructuredMapFromArrow(ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
	}
	return nil, nil
}

func buildListFromNativeArrow(ctx context.Context, rowIdx int, fieldMetadata query.FieldMetadata, srcValue arrow.Array, loc *time.Location, higherPrecision bool, params *syncParams) (snowflakeValue, error) {
	list, ok := srcValue.(*array.List)
	if !ok {
		return nil, unexpectedArrowStructured(srcValue, true, true, "ARRAY", "an Arrow list")
	}
	if list.IsNull(rowIdx) {
		return nil, nil
	}
	values := list.ListValues()
	offsets := list.Offsets()
	snowflakeType := types.GetSnowflakeType(fieldMetadata.Type)
	switch snowflakeType {
	case types.FixedType:
		switch typedValues := values.(type) {
		case *array.Decimal128:
			if higherPrecision && fieldMetadata.Scale == 0 {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*big.Int, error) {
					bigInt := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if bigInt == nil {
						return nil, nil
					}
					return bigInt.(*big.Int), nil

				})
			} else if higherPrecision && fieldMetadata.Scale != 0 {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*big.Float, error) {
					bigFloat := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if bigFloat == nil {
						return nil, nil
					}
					return bigFloat.(*big.Float), nil

				})

			} else if !higherPrecision && fieldMetadata.Scale == 0 {
				if embeddedValuesNullableEnabled(ctx) {
					return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullInt64, error) {
						v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
						if v == nil {
							return sql.NullInt64{Valid: false}, nil
						}
						val, err := strconv.ParseInt(v.(string), 10, 64)
						if err != nil {
							return sql.NullInt64{Valid: false}, err
						}
						return sql.NullInt64{Valid: true, Int64: val}, nil

					})
				}
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int64, error) {
					v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if v == nil {
						return 0, errors2.ErrNullValueInArrayError()
					}
					return strconv.ParseInt(v.(string), 10, 64)
				})
			} else {
				if embeddedValuesNullableEnabled(ctx) {
					return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullFloat64, error) {
						v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
						if v == nil {
							return sql.NullFloat64{Valid: false}, nil
						}
						val, err := strconv.ParseFloat(v.(string), 64)
						if err != nil {
							return sql.NullFloat64{Valid: false}, err
						}
						return sql.NullFloat64{Valid: true, Float64: val}, nil

					})
				}
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (float64, error) {
					v := arrowDecimal128ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if v == nil {
						return 0, errors2.ErrNullValueInArrayError()
					}
					return strconv.ParseFloat(v.(string), 64)
				})

			}
		case *array.Int64:
			if embeddedValuesNullableEnabled(ctx) {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullInt64, error) {
					resInt := arrowInt64ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if resInt == nil {
						return sql.NullInt64{Valid: false}, nil
					}
					return sql.NullInt64{Valid: true, Int64: resInt.(int64)}, nil
				})
			}
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int64, error) {
				resInt := arrowInt64ToValue(typedValues, j, higherPrecision, fieldMetadata)
				if resInt == nil {
					return 0, errors2.ErrNullValueInArrayError()
				}
				return resInt.(int64), nil
			})

		case *array.Int32:
			if embeddedValuesNullableEnabled(ctx) {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullInt32, error) {
					resInt := arrowInt32ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if resInt == nil {
						return sql.NullInt32{Valid: false}, nil
					}
					return sql.NullInt32{Valid: true, Int32: resInt.(int32)}, nil

				})
			}
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int32, error) {
				resInt := arrowInt32ToValue(typedValues, j, higherPrecision, fieldMetadata)
				if resInt == nil {
					return 0, errors2.ErrNullValueInArrayError()
				}
				return resInt.(int32), nil
			})
		case *array.Int16:
			if embeddedValuesNullableEnabled(ctx) {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullInt16, error) {
					resInt := arrowInt16ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if resInt == nil {
						return sql.NullInt16{Valid: false}, nil
					}
					return sql.NullInt16{Valid: true, Int16: resInt.(int16)}, nil

				})
			}
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int16, error) {
				resInt := arrowInt16ToValue(typedValues, j, higherPrecision, fieldMetadata)
				if resInt == nil {
					return 0, errors2.ErrNullValueInArrayError()
				}
				return resInt.(int16), nil
			})

		case *array.Int8:
			if embeddedValuesNullableEnabled(ctx) {
				return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullByte, error) {
					resInt := arrowInt8ToValue(typedValues, j, higherPrecision, fieldMetadata)
					if resInt == nil {
						return sql.NullByte{Valid: false}, nil
					}
					return sql.NullByte{Valid: true, Byte: resInt.(byte)}, nil
				})
			}
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (int8, error) {
				resInt := arrowInt8ToValue(typedValues, j, higherPrecision, fieldMetadata)
				if resInt == nil {
					return 0, errors2.ErrNullValueInArrayError()
				}
				return resInt.(int8), nil
			})
		}
	case types.RealType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullFloat64, error) {
				resFloat := arrowRealToValue(values.(*array.Float64), j)
				if resFloat == nil {
					return sql.NullFloat64{Valid: false}, nil
				}
				return sql.NullFloat64{Valid: true, Float64: resFloat.(float64)}, nil
			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (float64, error) {
			resFloat := arrowRealToValue(values.(*array.Float64), j)
			if resFloat == nil {
				return 0, errors2.ErrNullValueInArrayError()
			}
			return resFloat.(float64), nil
		})
	case types.TextType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullString, error) {
				resString := arrowStringToValue(values.(*array.String), j)
				if resString == nil {
					return sql.NullString{Valid: false}, nil
				}
				return sql.NullString{Valid: true, String: resString.(string)}, nil
			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (string, error) {
			resString := arrowStringToValue(values.(*array.String), j)
			if resString == nil {
				return "", errors2.ErrNullValueInArrayError()
			}
			return resString.(string), nil
		})
	case types.BooleanType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullBool, error) {
				resBool := arrowBoolToValue(values.(*array.Boolean), j)
				if resBool == nil {
					return sql.NullBool{Valid: false}, nil
				}
				return sql.NullBool{Valid: true, Bool: resBool.(bool)}, nil
			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (bool, error) {
			resBool := arrowBoolToValue(values.(*array.Boolean), j)
			if resBool == nil {
				return false, errors2.ErrNullValueInArrayError()
			}
			return resBool.(bool), nil

		})

	case types.BinaryType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) ([]byte, error) {
			res := arrowBinaryToValue(values.(*array.Binary), j)
			if res == nil {
				return nil, nil
			}
			return res.([]byte), nil

		})
	case types.DateType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullTime, error) {
				v := arrowDateToValue(values.(*array.Date32), j)
				if v == nil {
					return sql.NullTime{Valid: false}, nil
				}
				return sql.NullTime{Valid: true, Time: v.(time.Time)}, nil

			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			v := arrowDateToValue(values.(*array.Date32), j)
			if v == nil {
				return time.Time{}, errors2.ErrNullValueInArrayError()
			}
			return v.(time.Time), nil

		})

	case types.TimeType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullTime, error) {
				v := arrowTimeToValue(values, j, fieldMetadata.Scale)
				if v == nil {
					return sql.NullTime{Valid: false}, nil
				}
				return sql.NullTime{Valid: true, Time: v.(time.Time)}, nil

			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			v := arrowTimeToValue(values, j, fieldMetadata.Scale)
			if v == nil {
				return time.Time{}, errors2.ErrNullValueInArrayError()
			}
			return v.(time.Time), nil

		})

	case types.TimestampNtzType, types.TimestampLtzType, types.TimestampTzType:
		if embeddedValuesNullableEnabled(ctx) {
			return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (sql.NullTime, error) {
				ptr := arrowSnowflakeTimestampToTime(values, snowflakeType, fieldMetadata.Scale, j, loc)
				if ptr != nil {
					return sql.NullTime{Valid: true, Time: *ptr}, nil
				}
				return sql.NullTime{Valid: false}, nil
			})
		}
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (time.Time, error) {
			ptr := arrowSnowflakeTimestampToTime(values, snowflakeType, fieldMetadata.Scale, j, loc)
			if ptr != nil {
				return *ptr, nil
			}
			return time.Time{}, errors2.ErrNullValueInArrayError()
		})
	case types.ObjectType:
		return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) (*structuredType, error) {
			if values.IsNull(j) {
				return nil, nil
			}
			m := make(map[string]any, len(fieldMetadata.Fields))
			for fieldIdx, field := range fieldMetadata.Fields {
				m[field.Name] = values.(*array.Struct).Field(fieldIdx).ValueStr(j)
			}
			return buildStructuredTypeRecursive(ctx, m, fieldMetadata.Fields, params)
		})
	case types.ArrayType:
		switch fieldMetadata.Fields[0].Type {
		case "text":
			if embeddedValuesNullableEnabled(ctx) {
				return buildArrowListRecursive[sql.NullString](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[string](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "fixed":
			if fieldMetadata.Fields[0].Scale == 0 {
				if embeddedValuesNullableEnabled(ctx) {
					return buildArrowListRecursive[sql.NullInt64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
				}
				return buildArrowListRecursive[int64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			if embeddedValuesNullableEnabled(ctx) {
				return buildArrowListRecursive[sql.NullFloat64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[float64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "real":
			if embeddedValuesNullableEnabled(ctx) {
				return buildArrowListRecursive[sql.NullFloat64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[float64](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "boolean":
			if embeddedValuesNullableEnabled(ctx) {
				return buildArrowListRecursive[sql.NullBool](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[bool](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "binary":
			return buildArrowListRecursive[[]byte](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			if embeddedValuesNullableEnabled(ctx) {
				return buildArrowListRecursive[sql.NullTime](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
			}
			return buildArrowListRecursive[time.Time](ctx, rowIdx, fieldMetadata, offsets, values, loc, higherPrecision, params)
		}
	}
	return nil, nil
}

func buildArrowListRecursive[T any](ctx context.Context, rowIdx int, fieldMetadata query.FieldMetadata, offsets []int32, values arrow.Array, loc *time.Location, higherPrecision bool, params *syncParams) (snowflakeValue, error) {
	return mapStructuredArrayNativeArrowRows(offsets, rowIdx, func(j int) ([]T, error) {
		arrowList, err := buildListFromNativeArrow(ctx, j, fieldMetadata.Fields[0], values, loc, higherPrecision, params)
		if err != nil {
			return nil, err
		}
		if arrowList == nil {
			return nil, nil
		}
		return arrowList.([]T), nil

	})
}

func mapStructuredArrayNativeArrowRows[T any](offsets []int32, rowIdx int, createValueFunc func(j int) (T, error)) (snowflakeValue, error) {
	arr := make([]T, offsets[rowIdx+1]-offsets[rowIdx])
	for j := offsets[rowIdx]; j < offsets[rowIdx+1]; j++ {
		v, err := createValueFunc(int(j))
		if err != nil {
			return nil, err
		}
		arr[j-offsets[rowIdx]] = v
	}
	return arr, nil
}

func extractInt64(values arrow.Array, j int) (int64, error) {
	switch typedValues := values.(type) {
	case *array.Decimal128:
		return int64(typedValues.Value(j).LowBits()), nil
	case *array.Int64:
		return typedValues.Value(j), nil
	case *array.Int32:
		return int64(typedValues.Value(j)), nil
	case *array.Int16:
		return int64(typedValues.Value(j)), nil
	case *array.Int8:
		return int64(typedValues.Value(j)), nil
	}
	return 0, fmt.Errorf("unsupported map type: %T", values.DataType().Name())
}

func buildStructuredMapFromArrow[K comparable](ctx context.Context, rowIdx int, valueMetadata query.FieldMetadata, offsets []int32, keyFunc func(j int) (K, error), items arrow.Array, higherPrecision bool, loc *time.Location, params *syncParams) (snowflakeValue, error) {
	mapNullValuesEnabled := embeddedValuesNullableEnabled(ctx)
	switch valueMetadata.Type {
	case "text":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullString), offsets, rowIdx, keyFunc, func(j int) (sql.NullString, error) {
				if items.IsNull(j) {
					return sql.NullString{Valid: false}, nil
				}
				return sql.NullString{Valid: true, String: items.(*array.String).Value(j)}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]string), offsets, rowIdx, keyFunc, func(j int) (string, error) {
			if items.IsNull(j) {
				return "", errors2.ErrNullValueInMapError()
			}
			return items.(*array.String).Value(j), nil
		})
	case "boolean":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullBool), offsets, rowIdx, keyFunc, func(j int) (sql.NullBool, error) {
				if items.IsNull(j) {
					return sql.NullBool{Valid: false}, nil
				}
				return sql.NullBool{Valid: true, Bool: items.(*array.Boolean).Value(j)}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]bool), offsets, rowIdx, keyFunc, func(j int) (bool, error) {
			if items.IsNull(j) {
				return false, errors2.ErrNullValueInMapError()
			}
			return items.(*array.Boolean).Value(j), nil
		})
	case "fixed":
		if higherPrecision && valueMetadata.Scale == 0 {
			return mapStructuredMapNativeArrowRows(make(map[K]*big.Int), offsets, rowIdx, keyFunc, func(j int) (*big.Int, error) {
				if items.IsNull(j) {
					return nil, nil
				}
				return mapStructuredMapNativeArrowFixedValue[*big.Int](valueMetadata, j, items, higherPrecision, nil)
			})
		} else if higherPrecision && valueMetadata.Scale != 0 {
			return mapStructuredMapNativeArrowRows(make(map[K]*big.Float), offsets, rowIdx, keyFunc, func(j int) (*big.Float, error) {
				if items.IsNull(j) {
					return nil, nil
				}
				return mapStructuredMapNativeArrowFixedValue[*big.Float](valueMetadata, j, items, higherPrecision, nil)
			})
		} else if !higherPrecision && valueMetadata.Scale == 0 {
			if mapNullValuesEnabled {
				return mapStructuredMapNativeArrowRows(make(map[K]sql.NullInt64), offsets, rowIdx, keyFunc, func(j int) (sql.NullInt64, error) {
					if items.IsNull(j) {
						return sql.NullInt64{Valid: false}, nil
					}
					s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
					if err != nil {
						return sql.NullInt64{}, err
					}
					i64, err := strconv.ParseInt(s, 10, 64)
					return sql.NullInt64{Valid: true, Int64: i64}, err
				})
			}
			return mapStructuredMapNativeArrowRows(make(map[K]int64), offsets, rowIdx, keyFunc, func(j int) (int64, error) {
				if items.IsNull(j) {
					return 0, errors2.ErrNullValueInMapError()
				}
				s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
				if err != nil {
					return 0, err
				}
				return strconv.ParseInt(s, 10, 64)
			})
		} else {
			if mapNullValuesEnabled {
				return mapStructuredMapNativeArrowRows(make(map[K]sql.NullFloat64), offsets, rowIdx, keyFunc, func(j int) (sql.NullFloat64, error) {
					if items.IsNull(j) {
						return sql.NullFloat64{Valid: false}, nil
					}
					s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
					if err != nil {
						return sql.NullFloat64{}, err
					}
					f64, err := strconv.ParseFloat(s, 64)
					return sql.NullFloat64{Valid: true, Float64: f64}, err
				})
			}
			return mapStructuredMapNativeArrowRows(make(map[K]float64), offsets, rowIdx, keyFunc, func(j int) (float64, error) {
				if items.IsNull(j) {
					return 0, errors2.ErrNullValueInMapError()
				}
				s, err := mapStructuredMapNativeArrowFixedValue[string](valueMetadata, j, items, higherPrecision, "")
				if err != nil {
					return 0, err
				}
				return strconv.ParseFloat(s, 64)
			})
		}
	case "real":
		if mapNullValuesEnabled {
			return mapStructuredMapNativeArrowRows(make(map[K]sql.NullFloat64), offsets, rowIdx, keyFunc, func(j int) (sql.NullFloat64, error) {
				if items.IsNull(j) {
					return sql.NullFloat64{Valid: false}, nil
				}
				f64 := items.(*array.Float64).Value(j)
				return sql.NullFloat64{Valid: true, Float64: f64}, nil
			})
		}
		return mapStructuredMapNativeArrowRows(make(map[K]float64), offsets, rowIdx, keyFunc, func(j int) (float64, error) {
			if items.IsNull(j) {
				return 0, errors2.ErrNullValueInMapError()
			}
			return arrowRealToValue(items.(*array.Float64), j).(float64), nil
		})
	case "binary":
		return mapStructuredMapNativeArrowRows(make(map[K][]byte), offsets, rowIdx, keyFunc, func(j int) ([]byte, error) {
			if items.IsNull(j) {
				return nil, nil
			}
			return arrowBinaryToValue(items.(*array.Binary), j).([]byte), nil
		})
	case "date":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return arrowDateToValue(items.(*array.Date32), j).(time.Time)
		})
	case "time":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return arrowTimeToValue(items, j, valueMetadata.Scale).(time.Time)
		})
	case "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
		return buildTimeFromNativeArrowArray(mapNullValuesEnabled, offsets, rowIdx, keyFunc, items, func(j int) time.Time {
			return *arrowSnowflakeTimestampToTime(items, types.GetSnowflakeType(valueMetadata.Type), valueMetadata.Scale, j, loc)
		})
	case "object":
		return mapStructuredMapNativeArrowRows(make(map[K]*structuredType), offsets, rowIdx, keyFunc, func(j int) (*structuredType, error) {
			if items.IsNull(j) {
				return nil, nil
			}
			var err error
			m := make(map[string]any)
			for fieldIdx, field := range valueMetadata.Fields {
				snowflakeType := types.GetSnowflakeType(field.Type)
				m[field.Name], err = arrowToValue(ctx, j, field, items.(*array.Struct).Field(fieldIdx), loc, higherPrecision, params, snowflakeType)
				if err != nil {
					return nil, err
				}
			}
			return &structuredType{
				values:        m,
				fieldMetadata: valueMetadata.Fields,
				params:        params,
			}, nil
		})
	case "array":
		switch valueMetadata.Fields[0].Type {
		case "text":
			return buildListFromNativeArrowMap[K, string](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "fixed":
			if valueMetadata.Fields[0].Scale == 0 {
				return buildListFromNativeArrowMap[K, int64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
			}
			return buildListFromNativeArrowMap[K, float64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "real":
			return buildListFromNativeArrowMap[K, float64](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "binary":
			return buildListFromNativeArrowMap[K, []byte](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "boolean":
			return buildListFromNativeArrowMap[K, bool](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		case "date", "time", "timestamp_ltz", "timestamp_ntz", "timestamp_tz":
			return buildListFromNativeArrowMap[K, time.Time](ctx, rowIdx, valueMetadata, offsets, keyFunc, items, higherPrecision, loc, params)
		}
	}
	return nil, errors.New("Unsupported map value: " + valueMetadata.Type)
}

func buildListFromNativeArrowMap[K comparable, V any](ctx context.Context, rowIdx int, valueMetadata query.FieldMetadata, offsets []int32, keyFunc func(j int) (K, error), items arrow.Array, higherPrecision bool, loc *time.Location, params *syncParams) (snowflakeValue, error) {
	return mapStructuredMapNativeArrowRows(make(map[K][]V), offsets, rowIdx, keyFunc, func(j int) ([]V, error) {
		if items.IsNull(j) {
			return nil, nil
		}
		list, err := buildListFromNativeArrow(ctx, j, valueMetadata.Fields[0], items, loc, higherPrecision, params)
		if err != nil {
			return nil, err
		}
		return list.([]V), nil
	})
}

func buildTimeFromNativeArrowArray[K comparable](mapNullValuesEnabled bool, offsets []int32, rowIdx int, keyFunc func(j int) (K, error), items arrow.Array, buildTime func(j int) time.Time) (snowflakeValue, error) {
	if mapNullValuesEnabled {
		return mapStructuredMapNativeArrowRows(make(map[K]sql.NullTime), offsets, rowIdx, keyFunc, func(j int) (sql.NullTime, error) {
			if items.IsNull(j) {
				return sql.NullTime{Valid: false}, nil
			}
			return sql.NullTime{Valid: true, Time: buildTime(j)}, nil
		})
	}
	return mapStructuredMapNativeArrowRows(make(map[K]time.Time), offsets, rowIdx, keyFunc, func(j int) (time.Time, error) {
		if items.IsNull(j) {
			return time.Time{}, errors2.ErrNullValueInMapError()
		}
		return buildTime(j), nil
	})
}

func mapStructuredMapNativeArrowFixedValue[V any](valueMetadata query.FieldMetadata, j int, items arrow.Array, higherPrecision bool, defaultValue V) (V, error) {
	v, err := extractNumberFromArrow(&items, j, higherPrecision, valueMetadata)
	if err != nil {
		return defaultValue, err
	}
	return v.(V), nil
}

func extractNumberFromArrow(values *arrow.Array, j int, higherPrecision bool, srcColumnMeta query.FieldMetadata) (snowflakeValue, error) {
	switch typedValues := (*values).(type) {
	case *array.Decimal128:
		return arrowDecimal128ToValue(typedValues, j, higherPrecision, srcColumnMeta), nil
	case *array.Int64:
		return arrowInt64ToValue(typedValues, j, higherPrecision, srcColumnMeta), nil
	case *array.Int32:
		return arrowInt32ToValue(typedValues, j, higherPrecision, srcColumnMeta), nil
	case *array.Int16:
		return arrowInt16ToValue(typedValues, j, higherPrecision, srcColumnMeta), nil
	case *array.Int8:
		return arrowInt8ToValue(typedValues, j, higherPrecision, srcColumnMeta), nil
	}
	return 0, fmt.Errorf("unknown number type: %T", values)
}

func mapStructuredMapNativeArrowRows[K comparable, V any](m map[K]V, offsets []int32, rowIdx int, keyFunc func(j int) (K, error), itemFunc func(j int) (V, error)) (map[K]V, error) {
	for j := offsets[rowIdx]; j < offsets[rowIdx+1]; j++ {
		k, err := keyFunc(int(j))
		if err != nil {
			return nil, err
		}
		if m[k], err = itemFunc(int(j)); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func arrowToStructuredType(ctx context.Context, structs *array.Struct, fieldMetadata []query.FieldMetadata, loc *time.Location, rowIdx int, higherPrecision bool, params *syncParams) (*structuredType, error) {
	var err error
	m := make(map[string]any)
	for colIdx := 0; colIdx < structs.NumField(); colIdx++ {
		var v any
		switch types.GetSnowflakeType(fieldMetadata[colIdx].Type) {
		case types.FixedType:
			v = structs.Field(colIdx).ValueStr(rowIdx)
			switch typedValues := structs.Field(colIdx).(type) {
			case *array.Decimal128:
				v = arrowDecimal128ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx])
			case *array.Int64:
				v = arrowInt64ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx])
			case *array.Int32:
				v = arrowInt32ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx])
			case *array.Int16:
				v = arrowInt16ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx])
			case *array.Int8:
				v = arrowInt8ToValue(typedValues, rowIdx, higherPrecision, fieldMetadata[colIdx])
			}
		case types.BooleanType:
			v = arrowBoolToValue(structs.Field(colIdx).(*array.Boolean), rowIdx)
		case types.RealType:
			v = arrowRealToValue(structs.Field(colIdx).(*array.Float64), rowIdx)
		case types.BinaryType:
			v = arrowBinaryToValue(structs.Field(colIdx).(*array.Binary), rowIdx)
		case types.DateType:
			v = arrowDateToValue(structs.Field(colIdx).(*array.Date32), rowIdx)
		case types.TimeType:
			v = arrowTimeToValue(structs.Field(colIdx), rowIdx, fieldMetadata[colIdx].Scale)
		case types.TextType:
			v = arrowStringToValue(structs.Field(colIdx).(*array.String), rowIdx)
		case types.TimestampLtzType, types.TimestampTzType, types.TimestampNtzType:
			ptr := arrowSnowflakeTimestampToTime(structs.Field(colIdx), types.GetSnowflakeType(fieldMetadata[colIdx].Type), fieldMetadata[colIdx].Scale, rowIdx, loc)
			if ptr != nil {
				v = *ptr
			}
		case types.ObjectType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				if v, err = arrowToStructuredType(ctx, structs.Field(colIdx).(*array.Struct), fieldMetadata[colIdx].Fields, loc, rowIdx, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		case types.ArrayType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				var err error
				if v, err = buildListFromNativeArrow(ctx, rowIdx, fieldMetadata[colIdx].Fields[0], structs.Field(colIdx), loc, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		case types.MapType:
			if !structs.Field(colIdx).IsNull(rowIdx) {
				var err error
				if v, err = buildMapFromNativeArrow(ctx, rowIdx, fieldMetadata[colIdx].Fields[0], fieldMetadata[colIdx].Fields[1], structs.Field(colIdx), loc, higherPrecision, params); err != nil {
					return nil, err
				}
			}
		}
		m[fieldMetadata[colIdx].Name] = v
	}
	return &structuredType{
		values:        m,
		fieldMetadata: fieldMetadata,
		params:        params,
	}, nil
}

func arrowStringToValue(srcValue *array.String, rowIdx int) snowflakeValue {
	if srcValue.IsNull(rowIdx) {
		return nil
	}
	return srcValue.Value(rowIdx)
}

func arrowDecimal128ToValue(srcValue *array.Decimal128, rowIdx int, higherPrecision bool, srcColumnMeta query.FieldMetadata) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		num := srcValue.Value(rowIdx)
		if srcColumnMeta.Scale == 0 {
			if higherPrecision {
				return num.BigInt()
			}
			return num.ToString(0)
		}
		if higherPrecision {
			return decimalToBigFloat(num, int64(srcColumnMeta.Scale))
		}
		return num.ToString(int32(srcColumnMeta.Scale))
	}
	return nil
}

func arrowInt64ToValue(srcValue *array.Int64, rowIdx int, higherPrecision bool, srcColumnMeta query.FieldMetadata) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(srcColumnMeta, higherPrecision, val)
	}
	return nil
}

func arrowInt32ToValue(srcValue *array.Int32, rowIdx int, higherPrecision bool, srcColumnMeta query.FieldMetadata) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(srcColumnMeta, higherPrecision, int64(val))
	}
	return nil
}

func arrowInt16ToValue(srcValue *array.Int16, rowIdx int, higherPrecision bool, srcColumnMeta query.FieldMetadata) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(srcColumnMeta, higherPrecision, int64(val))
	}
	return nil
}

func arrowInt8ToValue(srcValue *array.Int8, rowIdx int, higherPrecision bool, srcColumnMeta query.FieldMetadata) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		val := srcValue.Value(rowIdx)
		return arrowIntToValue(srcColumnMeta, higherPrecision, int64(val))
	}
	return nil
}

func arrowIntToValue(srcColumnMeta query.FieldMetadata, higherPrecision bool, val int64) snowflakeValue {
	if srcColumnMeta.Scale == 0 {
		if higherPrecision {
			if srcColumnMeta.Precision >= 19 {
				return big.NewInt(val)
			}
			return val
		}
		return strconv.FormatInt(val, 10)
	}
	if higherPrecision {
		f := intToBigFloat(val, int64(srcColumnMeta.Scale))
		return f
	}
	return decimal128.FromI64(val).ToString(int32(srcColumnMeta.Scale))
}

func arrowRealToValue(srcValue *array.Float64, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowDecFloatToValue(ctx context.Context, srcValue *array.Struct, rowIdx int) (snowflakeValue, error) {
	if !srcValue.IsNull(rowIdx) {
		exponent := srcValue.Field(0).(*array.Int16).Value(rowIdx)
		mantissaBytes := srcValue.Field(1).(*array.Binary).Value(rowIdx)
		mantissaInt, err := parseTwosComplementBigEndian(mantissaBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse mantissa bytes: %s, error: %v", hex.EncodeToString(mantissaBytes), err)
		}
		if decfloatMappingEnabled(ctx) {
			mantissa := new(big.Float).SetPrec(127).SetInt(mantissaInt)
			if result, ok := new(big.Float).SetPrec(127).SetString(fmt.Sprintf("%ve%v", mantissa.Text('G', 38), exponent)); ok {
				return result, nil
			}
			return nil, fmt.Errorf("failed to create decfloat from mantissa %s and exponent %d", mantissa.Text('G', 38), exponent)
		}
		mantissaStr := mantissaInt.String()
		if mantissaStr == "0" {
			return "0", nil
		}
		negative := mantissaStr[0] == '-'
		mantissaUnsigned := strings.TrimLeft(mantissaStr, "-")
		mantissaLen := len(mantissaUnsigned)
		if mantissaLen > 1 {
			mantissaUnsigned = mantissaUnsigned[0:1] + "." + mantissaUnsigned[1:]
		}
		if negative {
			mantissaStr = "-" + mantissaUnsigned
		} else {
			mantissaStr = mantissaUnsigned
		}
		exponent = exponent + int16(mantissaLen) - 1
		result := mantissaStr
		if exponent != 0 {
			result = mantissaStr + "e" + strconv.Itoa(int(exponent))
		}
		return result, nil
	}
	return nil, nil
}

func parseTwosComplementBigEndian(b []byte) (*big.Int, error) {
	if len(b) > 16 {
		return nil, fmt.Errorf("input byte slice is too long (max 16 bytes)")
	}

	val := new(big.Int)
	val.SetBytes(b) // big.Int.SetBytes treats the bytes as an unsigned magnitude

	// If the sign bit is 1, the number is negative.
	if b[0]&0x80 != 0 {
		// Calculate 2^(bit length) for subtraction
		bitLength := uint(len(b) * 8)
		powerOfTwo := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(bitLength)), nil)

		// Subtract 2^(bit length) from the unsigned value to get the signed value.
		val.Sub(val, powerOfTwo)
	}

	return val, nil
}

func arrowBoolToValue(srcValue *array.Boolean, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowBinaryToValue(srcValue *array.Binary, rowIdx int) snowflakeValue {
	if !srcValue.IsNull(rowIdx) {
		return srcValue.Value(rowIdx)
	}
	return nil
}

func arrowDateToValue(srcValue *array.Date32, rowID int) snowflakeValue {
	if !srcValue.IsNull(rowID) {
		return time.Unix(int64(srcValue.Value(rowID))*86400, 0).UTC()
	}
	return nil
}

func arrowTimeToValue(srcValue arrow.Array, rowIdx int, scale int) snowflakeValue {
	t0 := time.Time{}
	if srcValue.DataType().ID() == arrow.INT64 {
		if !srcValue.IsNull(rowIdx) {
			return t0.Add(time.Duration(srcValue.(*array.Int64).Value(rowIdx) * int64(math.Pow10(9-scale))))
		}
	} else {
		if !srcValue.IsNull(rowIdx) {
			return t0.Add(time.Duration(int64(srcValue.(*array.Int32).Value(rowIdx)) * int64(math.Pow10(9-scale))))
		}
	}
	return nil
}

type (
	intArray          []int
	int32Array        []int32
	int64Array        []int64
	float64Array      []float64
	float32Array      []float32
	decfloatArray     []*big.Float
	boolArray         []bool
	stringArray       []string
	byteArray         [][]byte
	timestampNtzArray []time.Time
	timestampLtzArray []time.Time
	timestampTzArray  []time.Time
	dateArray         []time.Time
	timeArray         []time.Time
)

// Array takes in a column of a row to be inserted via array binding, bulk or
// otherwise, and converts it into a native snowflake type for binding
func Array(a any, typ ...any) (any, error) {

	switch t := a.(type) {
	case []int:
		return (*intArray)(&t), nil
	case []int32:
		return (*int32Array)(&t), nil
	case []int64:
		return (*int64Array)(&t), nil
	case []float64:
		return (*float64Array)(&t), nil
	case []float32:
		return (*float32Array)(&t), nil
	case []*big.Float:
		if len(typ) == 1 {
			if b, ok := typ[0].([]byte); ok && bytes.Equal(b, DataTypeDecfloat) {
				return (*decfloatArray)(&t), nil
			}
		}
		return nil, errors.New("unsupported *big.Float array bind. Set the type to DataTypeDecfloat to use decfloatArray")
	case []bool:
		return (*boolArray)(&t), nil
	case []string:
		return (*stringArray)(&t), nil
	case [][]byte:
		return (*byteArray)(&t), nil
	case []time.Time:
		if len(typ) < 1 {
			return nil, errUnsupportedTimeArrayBind
		}
		switch typ[0] {
		case TimestampNTZType:
			return (*timestampNtzArray)(&t), nil
		case TimestampLTZType:
			return (*timestampLtzArray)(&t), nil
		case TimestampTZType:
			return (*timestampTzArray)(&t), nil
		case DateType:
			return (*dateArray)(&t), nil
		case TimeType:
			return (*timeArray)(&t), nil
		default:
			return nil, errUnsupportedTimeArrayBind
		}
	case *[]int:
		return (*intArray)(t), nil
	case *[]int32:
		return (*int32Array)(t), nil
	case *[]int64:
		return (*int64Array)(t), nil
	case *[]float64:
		return (*float64Array)(t), nil
	case *[]float32:
		return (*float32Array)(t), nil
	case *[]*big.Float:
		if len(typ) == 1 {
			if b, ok := typ[0].([]byte); ok && bytes.Equal(b, DataTypeDecfloat) {
				return (*decfloatArray)(t), nil
			}
		}
		return nil, errors.New("unsupported *big.Float array bind. Set the type to DataTypeDecfloat to use decfloatArray")
	case *[]bool:
		return (*boolArray)(t), nil
	case *[]string:
		return (*stringArray)(t), nil
	case *[][]byte:
		return (*byteArray)(t), nil
	case *[]time.Time:
		if len(typ) < 1 {
			return nil, errUnsupportedTimeArrayBind
		}
		switch typ[0] {
		case TimestampNTZType:
			return (*timestampNtzArray)(t), nil
		case TimestampLTZType:
			return (*timestampLtzArray)(t), nil
		case TimestampTZType:
			return (*timestampTzArray)(t), nil
		case DateType:
			return (*dateArray)(t), nil
		case TimeType:
			return (*timeArray)(t), nil
		default:
			return nil, errUnsupportedTimeArrayBind
		}
	case []any, *[]any:
		// Support for bulk array binding insertion using []any / *[]any
		if len(typ) < 1 {
			return interfaceArrayBinding{
				hasTimezone:       false,
				timezoneTypeArray: a,
			}, nil
		}
		return interfaceArrayBinding{
			hasTimezone:       true,
			tzType:            typ[0].(timezoneType),
			timezoneTypeArray: a,
		}, nil
	default:
		return nil, fmt.Errorf("unknown array type for binding: %T", a)
	}
}

// snowflakeArrayToString converts the array binding to snowflake's native
// string type. The string value differs whether it's directly bound or
// uploaded via stream.
func snowflakeArrayToString(nv *driver.NamedValue, stream bool) (types.SnowflakeType, []*string, error) {
	var t types.SnowflakeType
	var arr []*string
	switch reflect.TypeOf(nv.Value) {
	case reflect.TypeFor[*intArray]():
		t = types.FixedType
		a := nv.Value.(*intArray)
		for _, x := range *a {
			v := strconv.Itoa(x)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*int64Array]():
		t = types.FixedType
		a := nv.Value.(*int64Array)
		for _, x := range *a {
			v := strconv.FormatInt(x, 10)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*int32Array]():
		t = types.FixedType
		a := nv.Value.(*int32Array)
		for _, x := range *a {
			v := strconv.Itoa(int(x))
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*float64Array]():
		t = types.RealType
		a := nv.Value.(*float64Array)
		for _, x := range *a {
			v := fmt.Sprintf("%g", x)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*float32Array]():
		t = types.RealType
		a := nv.Value.(*float32Array)
		for _, x := range *a {
			v := fmt.Sprintf("%g", x)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*decfloatArray]():
		t = types.TextType
		a := nv.Value.(*decfloatArray)
		for _, x := range *a {
			v := x.Text('g', decfloatPrintingPrec)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*boolArray]():
		t = types.BooleanType
		a := nv.Value.(*boolArray)
		for _, x := range *a {
			v := strconv.FormatBool(x)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*stringArray]():
		t = types.TextType
		a := nv.Value.(*stringArray)
		for _, x := range *a {
			v := x // necessary for address to be not overwritten
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*byteArray]():
		t = types.BinaryType
		a := nv.Value.(*byteArray)
		for _, x := range *a {
			v := hex.EncodeToString(x)
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*timestampNtzArray]():
		t = types.TimestampNtzType
		a := nv.Value.(*timestampNtzArray)
		for _, x := range *a {
			v, err := getTimestampBindValue(x, stream, t)
			if err != nil {
				return types.UnSupportedType, nil, err
			}
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*timestampLtzArray]():
		t = types.TimestampLtzType
		a := nv.Value.(*timestampLtzArray)

		for _, x := range *a {
			v, err := getTimestampBindValue(x, stream, t)
			if err != nil {
				return types.UnSupportedType, nil, err
			}
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*timestampTzArray]():
		t = types.TimestampTzType
		a := nv.Value.(*timestampTzArray)
		for _, x := range *a {
			v, err := getTimestampBindValue(x, stream, t)
			if err != nil {
				return types.UnSupportedType, nil, err
			}
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*dateArray]():
		t = types.DateType
		a := nv.Value.(*dateArray)
		for _, x := range *a {
			var v string
			if stream {
				v = x.Format("2006-01-02")
			} else {
				_, offset := x.Zone()
				x = x.Add(time.Second * time.Duration(offset))
				v = strconv.FormatInt(x.Unix()*1000, 10)
			}
			arr = append(arr, &v)
		}
	case reflect.TypeFor[*timeArray]():
		t = types.TimeType
		a := nv.Value.(*timeArray)
		for _, x := range *a {
			var v string
			if stream {
				v = fmt.Sprintf("%02d:%02d:%02d.%09d", x.Hour(), x.Minute(), x.Second(), x.Nanosecond())
			} else {
				h, m, s := x.Clock()
				tm := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) + int64(s)*int64(time.Second) + int64(x.Nanosecond())
				v = strconv.FormatInt(tm, 10)
			}
			arr = append(arr, &v)
		}
	default:
		// Support for bulk array binding insertion using []any / *[]any
		nvValue := reflect.ValueOf(nv)
		if nvValue.Kind() == reflect.Pointer {
			value := reflect.Indirect(reflect.ValueOf(nv.Value))
			if isInterfaceArrayBinding(value.Interface()) {
				timeStruct, ok := value.Interface().(interfaceArrayBinding)
				if ok {
					timeInterfaceSlice := reflect.Indirect(reflect.ValueOf(timeStruct.timezoneTypeArray))
					if timeStruct.hasTimezone {
						return interfaceSliceToString(timeInterfaceSlice, stream, timeStruct.tzType)
					}
					return interfaceSliceToString(timeInterfaceSlice, stream)
				}
			}
		}
		return types.UnSupportedType, nil, nil
	}
	return t, arr, nil
}

func interfaceSliceToString(interfaceSlice reflect.Value, stream bool, tzType ...timezoneType) (types.SnowflakeType, []*string, error) {
	var t types.SnowflakeType
	var arr []*string

	for i := 0; i < interfaceSlice.Len(); i++ {
		val := interfaceSlice.Index(i)
		if val.CanInterface() {
			v := val.Interface()

			switch x := v.(type) {
			case int:
				t = types.FixedType
				v := strconv.Itoa(x)
				arr = append(arr, &v)
			case int32:
				t = types.FixedType
				v := strconv.Itoa(int(x))
				arr = append(arr, &v)
			case int64:
				t = types.FixedType
				v := strconv.FormatInt(x, 10)
				arr = append(arr, &v)
			case float32:
				t = types.RealType
				v := fmt.Sprintf("%g", x)
				arr = append(arr, &v)
			case float64:
				t = types.RealType
				v := fmt.Sprintf("%g", x)
				arr = append(arr, &v)
			case bool:
				t = types.BooleanType
				v := strconv.FormatBool(x)
				arr = append(arr, &v)
			case string:
				t = types.TextType
				arr = append(arr, &x)
			case []byte:
				t = types.BinaryType
				v := hex.EncodeToString(x)
				arr = append(arr, &v)
			case time.Time:
				if len(tzType) < 1 {
					return types.UnSupportedType, nil, nil
				}

				switch tzType[0] {
				case TimestampNTZType:
					t = types.TimestampNtzType
					v, err := getTimestampBindValue(x, stream, t)
					if err != nil {
						return types.UnSupportedType, nil, err
					}
					arr = append(arr, &v)
				case TimestampLTZType:
					t = types.TimestampLtzType
					v, err := getTimestampBindValue(x, stream, t)
					if err != nil {
						return types.UnSupportedType, nil, err
					}
					arr = append(arr, &v)
				case TimestampTZType:
					t = types.TimestampTzType
					v, err := getTimestampBindValue(x, stream, t)
					if err != nil {
						return types.UnSupportedType, nil, err
					}
					arr = append(arr, &v)
				case DateType:
					t = types.DateType
					_, offset := x.Zone()
					x = x.Add(time.Second * time.Duration(offset))
					v := strconv.FormatInt(x.Unix()*1000, 10)
					arr = append(arr, &v)
				case TimeType:
					t = types.TimeType
					var v string
					if stream {
						v = x.Format(format[11:19])
					} else {
						h, m, s := x.Clock()
						tm := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) + int64(s)*int64(time.Second) + int64(x.Nanosecond())
						v = strconv.FormatInt(tm, 10)
					}
					arr = append(arr, &v)
				default:
					return types.UnSupportedType, nil, nil
				}
			case driver.Valuer: // honor each driver's Valuer interface
				if value, err := x.Value(); err == nil && value != nil {
					// if the output value is a valid string, return that
					if strVal, ok := value.(string); ok {
						t = types.TextType
						arr = append(arr, &strVal)
					}
				} else if v != nil {
					return types.UnSupportedType, nil, nil
				} else {
					arr = append(arr, nil)
				}
			default:
				if val.Interface() != nil {
					if isUUIDImplementer(val) {
						t = types.TextType
						x := v.(fmt.Stringer).String()
						arr = append(arr, &x)
						continue
					}
					return types.UnSupportedType, nil, nil
				}

				arr = append(arr, nil)
			}
		}
	}
	return t, arr, nil
}

func higherPrecisionEnabled(ctx context.Context) bool {
	return ia.HigherPrecisionEnabled(ctx)
}

func decfloatMappingEnabled(ctx context.Context) bool {
	v := ctx.Value(enableDecfloat)
	if v == nil {
		return false
	}
	d, ok := v.(bool)
	return ok && d
}

// TypedNullTime is required to properly bind the null value with the snowflakeType as the Snowflake functions
// require the type of the field to be provided explicitly for the null values
type TypedNullTime struct {
	Time   sql.NullTime
	TzType timezoneType
}

func convertTzTypeToSnowflakeType(tzType timezoneType) types.SnowflakeType {
	switch tzType {
	case TimestampNTZType:
		return types.TimestampNtzType
	case TimestampLTZType:
		return types.TimestampLtzType
	case TimestampTZType:
		return types.TimestampTzType
	case DateType:
		return types.DateType
	case TimeType:
		return types.TimeType
	}
	return types.UnSupportedType
}

func getTimestampBindValue(x time.Time, stream bool, t types.SnowflakeType) (string, error) {
	if stream {
		return formatTimestampForStream(x, t), nil
	}
	return convertTimeToTimeStamp(x, t)
}

// formatTimestampForStream writes a CSV timestamp that preserves the same instant
// as convertTimeToTimeStamp, independent of session TIMEZONE. NTZ is rendered in UTC
// to match the UTC wallclock that Unix nanos imply. LTZ and TZ Format in x's own
// Location, not time.Local: that keeps the same instant as convertTimeToTimeStamp
// and does not depend on host or session timezone. JDBC re-expresses LTZ in the JVM
// default zone (equivalent for the instant); aligning this with time.Local would
// break callers who pass a non-local time.Time. TZ also stores that offset as data.
// The fixed nine-digit fraction keeps the output shape identical to the one JDBC's
// bind uploader has been sending.
func formatTimestampForStream(x time.Time, t types.SnowflakeType) string {
	if t == types.TimestampNtzType {
		x = x.UTC()
	}
	return x.Format(streamTimestampFormat)
}

func convertTimeToTimeStamp(x time.Time, t types.SnowflakeType) (string, error) {
	unixTime, _ := new(big.Int).SetString(strconv.FormatInt(x.Unix(), 10), 10)
	m, ok := new(big.Int).SetString(strconv.FormatInt(1e9, 10), 10)
	if !ok {
		return "", errors.New("failed to parse big int from string: invalid format or unsupported characters")
	}

	unixTime.Mul(unixTime, m)
	tmNanos, _ := new(big.Int).SetString(strconv.Itoa(x.Nanosecond()), 10)
	if t == types.TimestampTzType {
		_, offset := x.Zone()
		return fmt.Sprintf("%v %v", unixTime.Add(unixTime, tmNanos), offset/60+1440), nil
	}
	return unixTime.Add(unixTime, tmNanos).String(), nil
}

func decoderWithNumbersAsStrings(srcValue *string) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewBufferString(*srcValue))
	decoder.UseNumber()
	return decoder
}
