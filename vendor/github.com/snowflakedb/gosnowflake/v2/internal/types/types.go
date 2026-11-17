package types

import (
	"strings"
)

// SnowflakeType represents the various data types supported by Snowflake, including both standard and internal types used by the driver.
type SnowflakeType int

const (
	// FixedType represents the FIXED data type in Snowflake, which is a numeric type with a specified precision and scale.
	FixedType SnowflakeType = iota
	// RealType represents the REAL data type in Snowflake, which is a floating-point numeric type.
	RealType
	// DecfloatType represents the DECFLOAT data type in Snowflake, which is a decimal floating-point numeric type with high precision.
	DecfloatType
	// TextType represents the TEXT data type in Snowflake, which is a variable-length string type.
	TextType
	// DateType represents the DATE data type in Snowflake, which is used to store calendar dates (year, month, day).
	DateType
	// VariantType represents the VARIANT data type in Snowflake, which is a semi-structured data type that can store values of various types.
	VariantType
	// TimestampLtzType represents the TIMESTAMP_LTZ data type in Snowflake, which is a timestamp with local time zone information.
	TimestampLtzType
	// TimestampNtzType represents the TIMESTAMP_NTZ data type in Snowflake, which is a timestamp without time zone information.
	TimestampNtzType
	// TimestampTzType represents the TIMESTAMP_TZ data type in Snowflake, which is a timestamp with time zone information.
	TimestampTzType
	// ObjectType represents the OBJECT data type in Snowflake, which is a semi-structured data type that can store key-value pairs.
	ObjectType
	// ArrayType represents the ARRAY data type in Snowflake, which is a semi-structured data type that can store ordered lists of values.
	ArrayType
	// MapType represents the MAP data type in Snowflake, which is a semi-structured data type that can store key-value pairs with unique keys.
	MapType
	// BinaryType represents the BINARY data type in Snowflake, which is used to store binary data (byte arrays).
	BinaryType
	// TimeType represents the TIME data type in Snowflake, which is used to store time values (hour, minute, second).
	TimeType
	// BooleanType represents the BOOLEAN data type in Snowflake, which is used to store boolean values (true/false).
	BooleanType

	// NullType represents a null value type, used internally to represent null values in Snowflake.
	NullType
	// SliceType represents a slice type, used internally to represent slices of data in Snowflake.
	SliceType
	// ChangeType represents a change type, used internally to represent changes in data in Snowflake.
	ChangeType
	// UnSupportedType represents an unsupported type, used internally to represent types that are not supported by the driver.
	UnSupportedType
	// NilObjectType represents a nil object type, used internally to represent null objects in Snowflake.
	NilObjectType
	// NilArrayType represents a nil array type, used internally to represent null arrays in Snowflake.
	NilArrayType
	// NilMapType represents a nil map type, used internally to represent null maps in Snowflake.
	NilMapType
)

// SnowflakeToDriverType maps Snowflake data type names (as strings) to their corresponding SnowflakeType constants used internally by the driver.
// This mapping allows for easy conversion between the string representation of Snowflake types and the internal enumeration used by the driver for type handling.
var SnowflakeToDriverType = map[string]SnowflakeType{
	"FIXED":         FixedType,
	"REAL":          RealType,
	"DECFLOAT":      DecfloatType,
	"TEXT":          TextType,
	"DATE":          DateType,
	"VARIANT":       VariantType,
	"TIMESTAMP_LTZ": TimestampLtzType,
	"TIMESTAMP_NTZ": TimestampNtzType,
	"TIMESTAMP_TZ":  TimestampTzType,
	"OBJECT":        ObjectType,
	"ARRAY":         ArrayType,
	"MAP":           MapType,
	"BINARY":        BinaryType,
	"TIME":          TimeType,
	"BOOLEAN":       BooleanType,
	"NULL":          NullType,
	"SLICE":         SliceType,
	"CHANGE_TYPE":   ChangeType,
	"NOT_SUPPORTED": UnSupportedType}

// DriverTypeToSnowflake is the inverse mapping of SnowflakeToDriverType, allowing for conversion from SnowflakeType constants back to their string representations.
var DriverTypeToSnowflake = invertMap(SnowflakeToDriverType)

func invertMap(m map[string]SnowflakeType) map[SnowflakeType]string {
	inv := make(map[SnowflakeType]string)
	for k, v := range m {
		if _, ok := inv[v]; ok {
			panic("failed to create DriverTypeToSnowflake map due to duplicated values")
		}
		inv[v] = k
	}
	return inv
}

// Byte returns the byte representation of the SnowflakeType, which can be used for efficient type handling and comparisons within the driver.
func (st SnowflakeType) Byte() byte {
	return byte(st)
}

func (st SnowflakeType) String() string {
	return DriverTypeToSnowflake[st]
}

// GetSnowflakeType takes a string representation of a Snowflake data type and returns the corresponding SnowflakeType constant used internally by the driver.
func GetSnowflakeType(typ string) SnowflakeType {
	return SnowflakeToDriverType[strings.ToUpper(typ)]
}
