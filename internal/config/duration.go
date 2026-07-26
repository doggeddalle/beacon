package config

import (
	"time"
)

// Duration wraps time.Duration so it can be written in the TOML config as a
// human string like "15m", "3s" or "24h" instead of a raw nanosecond count.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText implements encoding.TextUnmarshaler (used by TOML) so values
// like "15m" parse correctly.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText implements encoding.TextMarshaler so the value is written back as
// a readable string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}
