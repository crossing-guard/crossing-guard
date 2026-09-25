package main

// doctorErrorSuffix renders an optional error text after a doctor line.
func doctorErrorSuffix(text string) string {
	if text == "" {
		return ""
	}
	return " — " + text
}
