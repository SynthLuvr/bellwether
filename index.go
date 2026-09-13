// Package bellwether will serve the last N daily closing prices of a
// stock — the Go port of forge-app. It is still the go-template
// scaffold: the placeholder helpers below keep the toolchain green
// until the port lands.
package bellwether

import "fmt"

// Greet returns a greeting.
func Greet() string {
	return "hello"
}

// ToLabel labels a string or an int.
func ToLabel[T string | int](value T) string {
	return fmt.Sprintf("label: %v", value)
}
