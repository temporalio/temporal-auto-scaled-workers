package iface

type (
	ScalingAlgorithmStatus map[string]any
)

func (state ScalingAlgorithmStatus) GetInt64Field(key string, defaultValue int64) int64 {
	return getInt64FromMap(state, key, defaultValue)
}

func (state ScalingAlgorithmStatus) GetFloat64Field(key string, defaultValue float64) float64 {
	return getFloat64FromMap(state, key, defaultValue)
}
