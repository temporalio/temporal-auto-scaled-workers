package iface

import (
	"strconv"

	"go.temporal.io/api/serviceerror"
)

func getInt64FromMap(m map[string]any, key string, defaultVal int64) int64 {
	if m == nil {
		return defaultVal
	}
	v, ok := m[key]
	if !ok || v == nil {
		return defaultVal
	}
	switch val := v.(type) {
	case int:
		return int64(val)
	case int64:
		return val
	case float64:
		return int64(val)
	case string:
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
		return defaultVal
	default:
		return defaultVal
	}
}

func getFloat64FromMap(m map[string]any, key string, defaultVal float64) float64 {
	if m == nil {
		return defaultVal
	}
	v, ok := m[key]
	if !ok || v == nil {
		return defaultVal
	}
	switch val := v.(type) {
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	case string:
		if i, err := strconv.ParseFloat(val, 64); err == nil {
			return i
		}
		return defaultVal
	default:
		return defaultVal
	}
}

func validateInt64InMap(m map[string]any, key string, minValidValue int64) error {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch val := v.(type) {
	case int:
		if int64(val) < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %d", key, minValidValue)
		}
	case int64:
		if val < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %d", key, minValidValue)
		}
	case float64:
		if val < float64(minValidValue) {
			return serviceerror.NewInvalidArgumentf("%s must be at least %d", key, minValidValue)
		}
	case string:
		i, err := strconv.ParseInt(val, 10, 64)
		if err != nil || i < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be an integer and at least %d", key, minValidValue)
		}
	default:
		return serviceerror.NewInvalidArgumentf("%s must be an integer and at least %d", key, minValidValue)
	}
	return nil
}

func validateFloat64InMap(m map[string]any, key string, minValidValue float64) error {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch val := v.(type) {
	case int:
		if float64(val) < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %v", key, minValidValue)
		}
	case int64:
		if float64(val) < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %v", key, minValidValue)
		}
	case float32:
		if float64(val) < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %v", key, minValidValue)
		}
	case float64:
		if val < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be at least %v", key, minValidValue)
		}
	case string:
		i, err := strconv.ParseFloat(val, 64)
		if err != nil || i < minValidValue {
			return serviceerror.NewInvalidArgumentf("%s must be a number and at least %d", key, minValidValue)
		}
	default:
		return serviceerror.NewInvalidArgumentf("%s must be a number and at least %v", key, minValidValue)
	}
	return nil
}
