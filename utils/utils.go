package utils

import (
	"fmt"
	"time"
)

func FormatETA(seconds float64) string {
	if seconds <= 0 || seconds > 31536000 { // > 1 year bounds check
		return "Calculating..."
	}
	d := time.Duration(seconds) * time.Second
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
	}
	return fmt.Sprintf("%02dm %02ds", m, s)
}
