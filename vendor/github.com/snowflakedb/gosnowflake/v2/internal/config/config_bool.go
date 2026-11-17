package config

// Bool is a type to represent true or false in the Config
type Bool uint8

const (
	// BoolNotSet represents the default value for the config field which is not set
	BoolNotSet Bool = iota // Reserved for unset to let default value fall into this category
	// BoolTrue represents true for the config field
	BoolTrue
	// BoolFalse represents false for the config field
	BoolFalse
)

func (cb Bool) String() string {
	switch cb {
	case BoolTrue:
		return "true"
	case BoolFalse:
		return "false"
	default:
		return "not set"
	}
}
