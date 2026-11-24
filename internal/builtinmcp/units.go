package builtinmcp

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var decimalAmount = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// unit is a chain's unit, 10^exp of its smallest.
type unit struct {
	name string
	exp  int
}

// convert converts in's amount exactly between native's units, smallest
// first, or a token's base and whole units.
func convert(in unitsIn, native []unit) (*mcp.CallToolResult, any, error) {
	var names []string
	for _, u := range native {
		names = append(names, u.name)
	}
	exp := func(name string) (int, error) {
		for _, u := range native {
			if u.name == name {
				return u.exp, nil
			}
		}
		switch name {
		case "base":
			return 0, nil
		case "whole":
			if in.Decimals == nil {
				return 0, errors.New("whole units need the token's decimals")
			}
			if *in.Decimals < 0 || *in.Decimals > 255 {
				return 0, errors.New("decimals are 0 to 255")
			}
			return *in.Decimals, nil
		}
		return 0, fmt.Errorf("no unit %q: the units are %s, base and whole", name, strings.Join(names, ", "))
	}
	from, err := exp(in.From)
	if err != nil {
		return nil, nil, err
	}
	to, err := exp(in.To)
	if err != nil {
		return nil, nil, err
	}
	amount := strings.TrimSpace(in.Amount)
	if !decimalAmount.MatchString(amount) {
		return nil, nil, errors.New("the amount is a decimal number such as 1.5, with no sign or exponent")
	}
	r, _ := new(big.Rat).SetString(amount)
	// in the smallest unit, which is indivisible
	r.Mul(r, pow10(from))
	if !r.IsInt() {
		smallest := native[0].name
		if in.From == "base" || in.From == "whole" {
			smallest = "base units"
		}
		return nil, nil, fmt.Errorf("%s %s is not a whole number of %s", amount, in.From, smallest)
	}
	r.Quo(r, pow10(to))
	return text(fmt.Sprintf("%s %s (%s %s)\n", decimalString(r, to), in.To, amount, in.From)), nil, nil
}

func pow10(n int) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
}

// decimalString is r, a whole number of 10^-places, exactly, without
// trailing zeros after the point.
func decimalString(r *big.Rat, places int) string {
	s := r.FloatString(places)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
