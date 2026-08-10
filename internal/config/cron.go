package config

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type cronFieldSpec struct {
	name  string
	min   int
	max   int
	names map[string]int
}

var cronFieldSpecs = []cronFieldSpec{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}},
	{name: "day of week", min: 0, max: 7, names: map[string]int{
		"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
	}},
}

// ValidateCronSchedule validates the five time fields accepted in an
// /etc/cron.d entry. It intentionally excludes scheduler-specific extensions
// such as nicknames, Quartz '?', L/W/#, and seconds/year fields.
func ValidateCronSchedule(schedule string) error {
	for _, r := range schedule {
		if (unicode.IsControl(r) && r != '\t') || (unicode.IsSpace(r) && r != ' ' && r != '\t') {
			return fmt.Errorf("must not contain line breaks or non-cron whitespace")
		}
	}
	if strings.ContainsRune(schedule, '?') {
		return fmt.Errorf("'?' is not supported by /etc/cron.d")
	}
	fields := strings.Fields(schedule)
	if len(fields) != len(cronFieldSpecs) {
		return fmt.Errorf("must contain exactly five fields")
	}
	for i, field := range fields {
		if err := validateCronField(field, cronFieldSpecs[i]); err != nil {
			return fmt.Errorf("%s field %q: %w", cronFieldSpecs[i].name, field, err)
		}
	}
	return nil
}

func validateCronField(field string, spec cronFieldSpec) error {
	items := strings.Split(field, ",")
	for _, item := range items {
		if item == "" {
			return fmt.Errorf("contains an empty list item")
		}
		base, stepText, hasStep, err := splitCronStep(item)
		if err != nil {
			return err
		}
		if base != "*" {
			if err := validateCronBase(base, spec); err != nil {
				return err
			}
			if hasStep && !strings.ContainsRune(base, '-') {
				return fmt.Errorf("step requires a wildcard or range")
			}
		}
		if hasStep {
			step, err := parseCronNumber(stepText)
			if err != nil || step <= 0 {
				return fmt.Errorf("step must be a positive integer")
			}
		}
	}
	return nil
}

func splitCronStep(item string) (base, step string, hasStep bool, err error) {
	parts := strings.Split(item, "/")
	switch len(parts) {
	case 1:
		return parts[0], "", false, nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", false, fmt.Errorf("malformed step")
		}
		return parts[0], parts[1], true, nil
	default:
		return "", "", false, fmt.Errorf("contains multiple step separators")
	}
}

func validateCronBase(base string, spec cronFieldSpec) error {
	rangeParts := strings.Split(base, "-")
	switch len(rangeParts) {
	case 1:
		_, err := parseCronValue(rangeParts[0], spec)
		return err
	case 2:
		if rangeParts[0] == "" || rangeParts[1] == "" {
			return fmt.Errorf("malformed range")
		}
		start, err := parseCronValue(rangeParts[0], spec)
		if err != nil {
			return err
		}
		end, err := parseCronValue(rangeParts[1], spec)
		if err != nil {
			return err
		}
		if start > end {
			return fmt.Errorf("range start must not exceed range end")
		}
		return nil
	default:
		return fmt.Errorf("malformed range")
	}
}

func parseCronValue(value string, spec cronFieldSpec) (int, error) {
	if named, ok := spec.names[strings.ToUpper(value)]; ok {
		return named, nil
	}
	number, err := parseCronNumber(value)
	if err != nil {
		return 0, fmt.Errorf("value %q must be a decimal integer", value)
	}
	if number < spec.min || number > spec.max {
		return 0, fmt.Errorf("value %d must be between %d and %d", number, spec.min, spec.max)
	}
	return number, nil
}

func parseCronNumber(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a decimal integer")
		}
	}
	return strconv.Atoi(value)
}
