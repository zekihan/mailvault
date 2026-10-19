package main

import (
	"errors"
	"time"
)

var errEpochTimeBound = errors.New("timestamp bound resolves to Unix epoch zero, which the source filter interface reserves for no bound")

func parseTimeBound(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	bound := t.Unix()
	// INTERNALDATE is second-resolution. For both inclusive lower and
	// exclusive upper comparisons, ceil preserves fractional RFC3339 bounds.
	if t.Nanosecond() != 0 {
		bound++
	}
	if bound == 0 {
		return 0, errEpochTimeBound
	}
	return bound, nil
}
