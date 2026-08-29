package config

import "fmt"

// fmtSscan wraps fmt.Sscan so config.go avoids importing fmt directly for a
// single numeric parse.
func fmtSscan(s string, dst ...any) (int, error) { return fmt.Sscan(s, dst...) }
