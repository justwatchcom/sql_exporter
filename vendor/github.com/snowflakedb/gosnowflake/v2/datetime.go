package gosnowflake

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var incorrectSecondsFractionRegex = regexp.MustCompile(`[^.,]FF`)
var correctSecondsFractionRegex = regexp.MustCompile(`FF(?P<fraction>\d?)`)

type formatReplacement struct {
	input  string
	output string
}

var formatReplacements = []formatReplacement{
	{input: "YYYY", output: "2006"},
	{input: "YY", output: "06"},
	{input: "MMMM", output: "January"},
	{input: "MM", output: "01"},
	{input: "MON", output: "Jan"},
	{input: "DD", output: "02"},
	{input: "DY", output: "Mon"},
	{input: "HH24", output: "15"},
	{input: "HH12", output: "03"},
	{input: "AM", output: "PM"},
	{input: "MI", output: "04"},
	{input: "SS", output: "05"},
	{input: "TZH", output: "Z07"},
	{input: "TZM", output: "00"},
}

func timeToString(t time.Time, dateTimeType string, sp *syncParams) (string, error) {
	sfFormat, err := dateTimeInputFormatByType(dateTimeType, sp)
	if err != nil {
		return "", err
	}
	goFormat, err := snowflakeFormatToGoFormat(sfFormat)
	if err != nil {
		return "", err
	}
	return t.Format(goFormat), nil
}

func snowflakeFormatToGoFormat(sfFormat string) (string, error) {
	res := sfFormat
	for _, replacement := range formatReplacements {
		res = strings.ReplaceAll(res, replacement.input, replacement.output)
	}

	if incorrectSecondsFractionRegex.MatchString(res) {
		return "", errors.New("incorrect second fraction - golang requires fraction to be preceded by comma or decimal point")
	}
	for {
		submatch := correctSecondsFractionRegex.FindStringSubmatch(res)
		if submatch == nil {
			break
		}
		fractionNumbers := 9
		if submatch[1] != "" {
			var err error
			fractionNumbers, err = strconv.Atoi(submatch[1])
			if err != nil {
				return "", err
			}
		}
		res = strings.ReplaceAll(res, submatch[0], strings.Repeat("0", fractionNumbers))
	}
	return res, nil
}

func dateTimeOutputFormatByType(dateTimeType string, sp *syncParams) (string, error) {
	var format *string
	switch strings.ToLower(dateTimeType) {
	case "date":
		format, _ = sp.get("date_output_format")
	case "time":
		format, _ = sp.get("time_output_format")
	case "timestamp_ltz":
		format, _ = sp.get("timestamp_ltz_output_format")
		if format == nil || *format == "" {
			format, _ = sp.get("timestamp_output_format")
		}
	case "timestamp_tz":
		format, _ = sp.get("timestamp_tz_output_format")
		if format == nil || *format == "" {
			format, _ = sp.get("timestamp_output_format")
		}
	case "timestamp_ntz":
		format, _ = sp.get("timestamp_ntz_output_format")
		if format == nil || *format == "" {
			format, _ = sp.get("timestamp_output_format")
		}
	}
	if format != nil {
		return *format, nil
	}
	return "", errors.New("not known output format parameter for " + dateTimeType)
}

func dateTimeInputFormatByType(dateTimeType string, sp *syncParams) (string, error) {
	var format *string
	var ok bool
	switch strings.ToLower(dateTimeType) {
	case "date":
		if format, ok = sp.get("date_input_format"); !ok || format == nil || *format == "" {
			format, _ = sp.get("date_output_format")
		}
	case "time":
		if format, ok = sp.get("time_input_format"); !ok || format == nil || *format == "" {
			format, _ = sp.get("time_output_format")
		}
	case "timestamp_ltz":
		if format, ok = sp.get("timestamp_ltz_input_format"); !ok || format == nil || *format == "" {
			if format, ok = sp.get("timestamp_input_format"); !ok || format == nil || *format == "" {
				if format, ok = sp.get("timestamp_ltz_output_format"); !ok || format == nil || *format == "" {
					format, _ = sp.get("timestamp_output_format")
				}
			}
		}
	case "timestamp_tz":
		if format, ok = sp.get("timestamp_tz_input_format"); !ok || format == nil || *format == "" {
			if format, ok = sp.get("timestamp_input_format"); !ok || format == nil || *format == "" {
				if format, ok = sp.get("timestamp_tz_output_format"); !ok || format == nil || *format == "" {
					format, _ = sp.get("timestamp_output_format")
				}
			}
		}
	case "timestamp_ntz":
		if format, ok = sp.get("timestamp_ntz_input_format"); !ok || format == nil || *format == "" {
			if format, ok = sp.get("timestamp_input_format"); !ok || format == nil || *format == "" {
				if format, ok = sp.get("timestamp_ntz_output_format"); !ok || format == nil || *format == "" {
					format, _ = sp.get("timestamp_output_format")
				}
			}
		}
	}
	if format != nil {
		return *format, nil
	}
	return "", errors.New("not known input format parameter for " + dateTimeType)
}
