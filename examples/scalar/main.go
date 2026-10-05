// Command scalar serves row-at-a-time Chalk external functions declared with
// chalkfn.Map1/Map2, which derive the Arrow schemas from the Go signatures.
package main

import (
	"errors"
	"strings"

	"github.com/chalk-ai/chalk-go-function/chalkfn"
)

func init() {
	chalkfn.Register(chalkfn.Map1("shout", "text", "result",
		func(s string) (string, error) { return strings.ToUpper(s) + "!", nil }))

	chalkfn.Register(chalkfn.Map2("safe_divide", "numerator", "denominator", "result",
		func(n, d float64) (float64, error) {
			if d == 0 {
				return 0, errors.New("division by zero")
			}
			return n / d, nil
		}))
}

func main() {
	chalkfn.Serve()
}
