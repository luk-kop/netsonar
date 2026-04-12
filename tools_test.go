//go:build tools

package tools

// Pin test-only dependencies so go mod tidy retains them.
import _ "github.com/leanovate/gopter"
