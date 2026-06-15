package agent

// TruncateObservation 给 observation 做统一截断
func TruncateObservation(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...<truncated>"

}
