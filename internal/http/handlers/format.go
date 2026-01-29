package handlers

import "time"

// timeLayout returns the Go time layout for the given preference.
// timeFormat: "12" or "24". Default "12".
func timeLayout(timeFormat string) string {
	if timeFormat == "24" {
		return "15:04:05"
	}
	return "3:04:05 PM"
}

// dateLayout returns the Go time layout for the given preference.
// dateFormat: "dd-mm-yyyy", "mm-dd-yyyy", "yyyy-mm-dd". Default "dd-mm-yyyy".
func dateLayout(dateFormat string) string {
	switch dateFormat {
	case "mm-dd-yyyy":
		return "01-02-2006"
	case "yyyy-mm-dd":
		return "2006-01-02"
	default:
		return "02-01-2006" // dd-mm-yyyy
	}
}

// FormatEventTime formats t for display as time only (e.g. recent events table).
func FormatEventTime(t time.Time, timeFormat string) string {
	if timeFormat == "" {
		timeFormat = "12"
	}
	return t.Format(timeLayout(timeFormat))
}

// FormatEventDateTime formats t for display as date and time (e.g. event detail).
func FormatEventDateTime(t time.Time, timeFormat, dateFormat string) string {
	if timeFormat == "" {
		timeFormat = "12"
	}
	if dateFormat == "" {
		dateFormat = "dd-mm-yyyy"
	}
	return t.Format(dateLayout(dateFormat) + " " + timeLayout(timeFormat))
}
