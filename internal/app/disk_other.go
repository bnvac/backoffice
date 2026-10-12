//go:build !unix

package app

// diskSpace has no portable implementation, so on other platforms the health
// page says the figure is unknown rather than showing a made-up one.
func diskSpace(string) (free, total int64) { return 0, 0 }
