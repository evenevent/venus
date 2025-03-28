package types

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	fbig "github.com/filecoin-project/go-state-types/big"

	"github.com/filecoin-project/venus/venus-shared/types/params"
)

var ZeroFIL = fbig.NewInt(0)

type FIL BigInt

func (f FIL) String() string {
	return f.Unitless() + " FIL"
}

var (
	AttoFil  = NewInt(1)
	FemtoFil = BigMul(AttoFil, NewInt(1000))
	PicoFil  = BigMul(FemtoFil, NewInt(1000))
	NanoFil  = BigMul(PicoFil, NewInt(1000))
)

func (f FIL) Unitless() string {
	r := new(big.Rat).SetFrac(f.Int, big.NewInt(int64(params.FilecoinPrecision)))
	if r.Sign() == 0 {
		return "0"
	}
	return strings.TrimRight(strings.TrimRight(r.FloatString(18), "0"), ".")
}

var unitPrefixes = []string{"a", "f", "p", "n", "μ", "m"}

func (f FIL) Short() string {
	n := BigInt(f).Abs()

	dn := uint64(1)
	var prefix string
	for _, p := range unitPrefixes {
		if n.LessThan(NewInt(dn * 1000)) {
			prefix = p
			break
		}
		dn *= 1000
	}

	r := new(big.Rat).SetFrac(f.Int, big.NewInt(int64(dn)))
	if r.Sign() == 0 {
		return "0"
	}

	return strings.TrimRight(strings.TrimRight(r.FloatString(3), "0"), ".") + " " + prefix + "FIL"
}

func (f FIL) Nano() string {
	r := new(big.Rat).SetFrac(f.Int, big.NewInt(int64(1e9)))
	if r.Sign() == 0 {
		return "0"
	}

	return strings.TrimRight(strings.TrimRight(r.FloatString(9), "0"), ".") + " nFIL"
}

func (f FIL) Format(s fmt.State, ch rune) {
	switch ch {
	case 's', 'v':
		_, _ = fmt.Fprint(s, f.String())
	default:
		f.Int.Format(s, ch)
	}
}

func (f FIL) MarshalText() (text []byte, err error) {
	return []byte(f.String()), nil
}

func (f *FIL) UnmarshalText(text []byte) error {
	p, err := ParseFIL(string(text))
	if err != nil {
		return err
	}

	if f.Int == nil {
		f.Int = big.NewInt(0)
	}

	f.Int.Set(p.Int)
	return nil
}

func (f FIL) MarshalJSON() ([]byte, error) {
	return []byte("\"" + f.String() + "\""), nil
}

func (f *FIL) UnmarshalJSON(by []byte) error {
	p, err := ParseFIL(strings.Trim(string(by), "\""))
	if err != nil {
		return err
	}
	if f.Int != nil {
		f.Int.Set(p.Int)
	} else {
		f.Int = p.Int
	}

	return nil
}

func ParseFIL(s string) (FIL, error) {
	suffix := strings.TrimLeft(s, "-.1234567890")
	s = s[:len(s)-len(suffix)]
	var attofil bool
	if suffix != "" {
		norm := strings.ToLower(strings.TrimSpace(suffix))
		switch norm {
		case "", "fil":
		case "attofil", "afil":
			attofil = true
		default:
			return FIL{}, fmt.Errorf("unrecognized suffix: %q", suffix)
		}
	}

	if len(s) > 50 {
		return FIL{}, fmt.Errorf("string length too large: %d", len(s))
	}

	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return FIL{}, fmt.Errorf("failed to parse %q as a decimal number", s)
	}

	if !attofil {
		r = r.Mul(r, big.NewRat(int64(params.FilecoinPrecision), 1))
	}

	if !r.IsInt() {
		var pref string
		if attofil {
			pref = "atto"
		}
		return FIL{}, fmt.Errorf("invalid %sFIL value: %q", pref, s)
	}

	return FIL{r.Num()}, nil
}

func MustParseFIL(s string) FIL {
	n, err := ParseFIL(s)
	if err != nil {
		panic(err)
	}

	return n
}

func FromFil(i uint64) BigInt {
	return BigMul(NewInt(i), NewInt(params.FilecoinPrecision))
}

var (
	_ encoding.TextMarshaler   = (*FIL)(nil)
	_ encoding.TextUnmarshaler = (*FIL)(nil)
)

var (
	_ json.Marshaler   = (*FIL)(nil)
	_ json.Unmarshaler = (*FIL)(nil)
)
