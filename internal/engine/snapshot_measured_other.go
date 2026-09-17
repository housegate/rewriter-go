//go:build !linux

package engine

import "fmt"

// NewMeasuredPolyglot is deliberately unavailable outside the qualified Linux
// identity mechanism. NewPolyglot retains ordinary platform behavior.
func NewMeasuredPolyglot(string) (Engine, MeasuredIdentity, error) {
	return nil, MeasuredIdentity{}, fmt.Errorf("engine: measured snapshot loading requires Linux")
}
