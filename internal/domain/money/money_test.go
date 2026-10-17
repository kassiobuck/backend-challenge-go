package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr error
	}{
		{name: "zero", input: "0.00", want: 0},
		{name: "integer", input: "25.00", want: 2500},
		{name: "fraction", input: "0.09", want: 9},
		{name: "negative internal difference", input: "-1.25", want: -125},
		{name: "max", input: "92233720368547758.07", want: math.MaxInt64},
		{name: "min", input: "-92233720368547758.08", want: math.MinInt64},
		{name: "empty", input: "", wantErr: domain.ErrInvalidMoney},
		{name: "missing fraction", input: "1", wantErr: domain.ErrInvalidMoney},
		{name: "one fractional digit", input: "1.0", wantErr: domain.ErrInvalidMoney},
		{name: "three fractional digits", input: "1.001", wantErr: domain.ErrInvalidMoney},
		{name: "scientific notation", input: "1e2", wantErr: domain.ErrInvalidMoney},
		{name: "nan", input: "NaN", wantErr: domain.ErrInvalidMoney},
		{name: "infinity", input: "Infinity", wantErr: domain.ErrInvalidMoney},
		{name: "leading whitespace", input: " 1.00", wantErr: domain.ErrInvalidMoney},
		{name: "trailing whitespace", input: "1.00 ", wantErr: domain.ErrInvalidMoney},
		{name: "plus sign", input: "+1.00", wantErr: domain.ErrInvalidMoney},
		{name: "multiple decimal separators", input: "1.0.0", wantErr: domain.ErrInvalidMoney},
		{name: "max overflow", input: "92233720368547758.08", wantErr: domain.ErrOverflow},
		{name: "min overflow", input: "-92233720368547758.09", wantErr: domain.ErrOverflow},
		{name: "parse integer overflow", input: "999999999999999999999999999999999999.00", wantErr: domain.ErrOverflow},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := ParseAmount(test.input, BRL)
			if test.wantErr != nil {
				if err == nil || !errors.Is(err, test.wantErr) {
					t.Fatalf("ParseAmount() error = %v, want errors.Is(_, %v)", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAmount() error = %v", err)
			}
			got, err := value.MinorUnits()
			if err != nil {
				t.Fatalf("MinorUnits() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("MinorUnits() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestParseNonNegativeAmountRejectsNegative(t *testing.T) {
	if _, err := ParseNonNegativeAmount("-0.01", BRL); !errors.Is(err, domain.ErrInvalidMoney) {
		t.Fatalf("ParseNonNegativeAmount() error = %v, want ErrInvalidMoney", err)
	}
}

func TestCurrencyValidation(t *testing.T) {
	if _, err := NewCurrency("JPY"); !errors.Is(err, domain.ErrUnsupportedCurrency) {
		t.Fatalf("NewCurrency(JPY) error = %v, want ErrUnsupportedCurrency", err)
	}
	if _, err := NewMinor(0, Currency("brl")); !errors.Is(err, domain.ErrInvalidCurrency) {
		t.Fatalf("NewMinor with invalid currency error = %v, want ErrInvalidCurrency", err)
	}
	if _, err := ParseAmount("1.00", Currency("")); !errors.Is(err, domain.ErrInvalidCurrency) {
		t.Fatalf("ParseAmount with invalid currency error = %v, want ErrInvalidCurrency", err)
	}
}

func TestMoneyArithmetic(t *testing.T) {
	one, _ := NewMinor(1, BRL)
	two, _ := NewMinor(2, BRL)
	negativeOne, _ := NewMinor(-1, BRL)

	sum, err := one.Add(two)
	if err != nil || sum.String() != "0.03" {
		t.Fatalf("Add() = %s, %v; want 0.03, nil", sum, err)
	}
	difference, err := one.Sub(two)
	if err != nil || difference.String() != "-0.01" {
		t.Fatalf("Sub() = %s, %v; want -0.01, nil", difference, err)
	}
	negated, err := negativeOne.Negate()
	if err != nil || negated.String() != "0.01" {
		t.Fatalf("Negate() = %s, %v; want 0.01, nil", negated, err)
	}
	comparison, err := one.Compare(two)
	if err != nil || comparison != -1 {
		t.Fatalf("Compare() = %d, %v; want -1, nil", comparison, err)
	}
}

func TestMoneyOverflow(t *testing.T) {
	max, _ := NewMinor(math.MaxInt64, BRL)
	min, _ := NewMinor(math.MinInt64, BRL)
	one, _ := NewMinor(1, BRL)
	negativeOne, _ := NewMinor(-1, BRL)

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "addition positive overflow", run: func() error { _, err := max.Add(one); return err }},
		{name: "subtraction positive overflow", run: func() error { _, err := max.Sub(negativeOne); return err }},
		{name: "subtraction negative overflow", run: func() error { _, err := min.Sub(one); return err }},
		{name: "negating min", run: func() error { _, err := min.Negate(); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, domain.ErrOverflow) {
				t.Fatalf("operation error = %v, want ErrOverflow", err)
			}
		})
	}
}

func TestMoneyRejectsCurrencyMismatchAndInvalidZeroValue(t *testing.T) {
	valid, _ := NewMinor(100, BRL)
	invalid := Money{}

	if _, err := valid.Add(invalid); !errors.Is(err, domain.ErrInvalidMoney) {
		t.Fatalf("Add with invalid money error = %v, want ErrInvalidMoney", err)
	}
	if _, err := invalid.MinorUnits(); !errors.Is(err, domain.ErrInvalidMoney) {
		t.Fatalf("MinorUnits on zero value error = %v, want ErrInvalidMoney", err)
	}
	usd, _ := NewMinor(100, USD)
	if _, err := valid.Add(usd); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("Add with different currencies error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestMoneyJSONUsesFixedDecimalString(t *testing.T) {
	value, _ := NewMinor(2505, BRL)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got, want := string(data), `{"amount":"25.05","currency":"BRL"}`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}

	var decoded Money
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.String() != "25.05" || !decoded.IsValid() {
		t.Fatalf("decoded money = %#v, want 25.05 BRL", decoded)
	}
}

func TestMoneyRoundTripProperty(t *testing.T) {
	for minor := int64(-100_000); minor <= 100_000; minor += 137 {
		value, err := NewMinor(minor, BRL)
		if err != nil {
			t.Fatalf("NewMinor(%d) error = %v", minor, err)
		}
		parsed, err := ParseAmount(value.String(), BRL)
		if err != nil {
			t.Fatalf("ParseAmount(%q) error = %v", value.String(), err)
		}
		got, _ := parsed.MinorUnits()
		if got != minor {
			t.Fatalf("round trip minor units = %d, want %d", got, minor)
		}
	}
}
