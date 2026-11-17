package gosnowflake

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"github.com/snowflakedb/gosnowflake/v2/internal/errors"
	"github.com/snowflakedb/gosnowflake/v2/internal/types"
)

var (
	// DataTypeFixed is a FIXED datatype.
	DataTypeFixed = []byte{types.FixedType.Byte()}
	// DataTypeReal is a REAL datatype.
	DataTypeReal = []byte{types.RealType.Byte()}
	// DataTypeDecfloat is a DECFLOAT datatype.
	DataTypeDecfloat = []byte{types.DecfloatType.Byte()}
	// DataTypeText is a TEXT datatype.
	DataTypeText = []byte{types.TextType.Byte()}
	// DataTypeDate is a Date datatype.
	DataTypeDate = []byte{types.DateType.Byte()}
	// DataTypeVariant is a TEXT datatype.
	DataTypeVariant = []byte{types.VariantType.Byte()}
	// DataTypeTimestampLtz is a TIMESTAMP_LTZ datatype.
	DataTypeTimestampLtz = []byte{types.TimestampLtzType.Byte()}
	// DataTypeTimestampNtz is a TIMESTAMP_NTZ datatype.
	DataTypeTimestampNtz = []byte{types.TimestampNtzType.Byte()}
	// DataTypeTimestampTz is a TIMESTAMP_TZ datatype.
	DataTypeTimestampTz = []byte{types.TimestampTzType.Byte()}
	// DataTypeObject is a OBJECT datatype.
	DataTypeObject = []byte{types.ObjectType.Byte()}
	// DataTypeArray is a ARRAY datatype.
	DataTypeArray = []byte{types.ArrayType.Byte()}
	// DataTypeBinary is a BINARY datatype.
	DataTypeBinary = []byte{types.BinaryType.Byte()}
	// DataTypeTime is a TIME datatype.
	DataTypeTime = []byte{types.TimeType.Byte()}
	// DataTypeBoolean is a BOOLEAN datatype.
	DataTypeBoolean = []byte{types.BooleanType.Byte()}
	// DataTypeNilObject represents a nil structured object.
	DataTypeNilObject = []byte{types.NilObjectType.Byte()}
	// DataTypeNilArray represents a nil structured array.
	DataTypeNilArray = []byte{types.NilArrayType.Byte()}
	// DataTypeNilMap represents a nil structured map.
	DataTypeNilMap = []byte{types.NilMapType.Byte()}
)

// dataTypeMode returns the subsequent data type in a string representation.
func dataTypeMode(v driver.Value) (tsmode types.SnowflakeType, err error) {
	if bd, ok := v.([]byte); ok {
		switch {
		case bytes.Equal(bd, DataTypeDecfloat):
			tsmode = types.DecfloatType
		case bytes.Equal(bd, DataTypeDate):
			tsmode = types.DateType
		case bytes.Equal(bd, DataTypeTime):
			tsmode = types.TimeType
		case bytes.Equal(bd, DataTypeTimestampLtz):
			tsmode = types.TimestampLtzType
		case bytes.Equal(bd, DataTypeTimestampNtz):
			tsmode = types.TimestampNtzType
		case bytes.Equal(bd, DataTypeTimestampTz):
			tsmode = types.TimestampTzType
		case bytes.Equal(bd, DataTypeBinary):
			tsmode = types.BinaryType
		case bytes.Equal(bd, DataTypeObject):
			tsmode = types.ObjectType
		case bytes.Equal(bd, DataTypeArray):
			tsmode = types.ArrayType
		case bytes.Equal(bd, DataTypeVariant):
			tsmode = types.VariantType
		case bytes.Equal(bd, DataTypeNilObject):
			tsmode = types.NilObjectType
		case bytes.Equal(bd, DataTypeNilArray):
			tsmode = types.NilArrayType
		case bytes.Equal(bd, DataTypeNilMap):
			tsmode = types.NilMapType
		default:
			return types.NullType, fmt.Errorf(errors.ErrMsgInvalidByteArray, v)
		}
	} else {
		return types.NullType, fmt.Errorf(errors.ErrMsgInvalidByteArray, v)
	}
	return tsmode, nil
}

// SnowflakeParameter includes the columns output from SHOW PARAMETER command.
type SnowflakeParameter struct {
	Key                       string
	Value                     string
	Default                   string
	Level                     string
	Description               string
	SetByUser                 string
	SetInJob                  string
	SetOn                     string
	SetByThreadID             string
	SetByThreadName           string
	SetByClass                string
	ParameterComment          string
	Type                      string
	IsExpired                 string
	ExpiresAt                 string
	SetByControllingParameter string
	ActivateVersion           string
	PartialRollout            string
	Unknown                   string // Reserve for added parameter
}

func populateSnowflakeParameter(colname string, p *SnowflakeParameter) any {
	switch colname {
	case "key":
		return &p.Key
	case "value":
		return &p.Value
	case "default":
		return &p.Default
	case "level":
		return &p.Level
	case "description":
		return &p.Description
	case "set_by_user":
		return &p.SetByUser
	case "set_in_job":
		return &p.SetInJob
	case "set_on":
		return &p.SetOn
	case "set_by_thread_id":
		return &p.SetByThreadID
	case "set_by_thread_name":
		return &p.SetByThreadName
	case "set_by_class":
		return &p.SetByClass
	case "parameter_comment":
		return &p.ParameterComment
	case "type":
		return &p.Type
	case "is_expired":
		return &p.IsExpired
	case "expires_at":
		return &p.ExpiresAt
	case "set_by_controlling_parameter":
		return &p.SetByControllingParameter
	case "activate_version":
		return &p.ActivateVersion
	case "partial_rollout":
		return &p.PartialRollout
	default:
		logger.Debugf("unknown type: %v", colname)
		return &p.Unknown
	}
}

// ScanSnowflakeParameter binds SnowflakeParameter variable with an array of column buffer.
func ScanSnowflakeParameter(rows *sql.Rows) (*SnowflakeParameter, error) {
	var err error
	var columns []string
	columns, err = rows.Columns()
	if err != nil {
		return nil, err
	}
	colNum := len(columns)
	p := SnowflakeParameter{}
	cols := make([]any, colNum)
	for i := range colNum {
		cols[i] = populateSnowflakeParameter(columns[i], &p)
	}
	err = rows.Scan(cols...)
	return &p, err
}
