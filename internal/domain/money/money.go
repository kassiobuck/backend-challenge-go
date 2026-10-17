package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
)

type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

func NewCurrency(code string) (Currency, error) {
	switch Currency(code) {
	case BRL, USD, EUR:
		return Currency(code), nil
	default:
		return "", fmt.Errorf("%w: %q", domain.ErrUnsupportedCurrency, code)
	}
}

type Money struct {
	minor    int64
	currency Currency
	valid    bool
}

func Zero(currency Currency) (Money, error) {
	return NewMinor(0, currency)
}

func NewMinor(minor int64, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", domain.ErrInvalidCurrency, currency)
	}
	return Money{minor: minor, currency: currency, valid: true}, nil
}

func ParseAmount(amount string, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", domain.ErrInvalidCurrency, currency)
	}
	if len(amount) < 4 {
		return Money{}, fmt.Errorf("%w: amount must have two decimal places", domain.ErrInvalidMoney)
	}

	negative := amount[0] == '-'
	unsigned := amount
	if negative {
		unsigned = amount[1:]
	}
	separator := strings.IndexByte(unsigned, '.')
	if separator < 1 || separator != len(unsigned)-3 || strings.IndexByte(unsigned[separator+1:], '.') >= 0 {
		return Money{}, fmt.Errorf("%w: amount must use fixed two-place decimal notation", domain.ErrInvalidMoney)
	}

	wholePart := unsigned[:separator]
	fractionPart := unsigned[separator+1:]
	for _, part := range []string{wholePart, fractionPart} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return Money{}, fmt.Errorf("%w: amount must contain decimal digits only", domain.ErrInvalidMoney)
			}
		}
	}

	whole, err := strconv.ParseUint(wholePart, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return Money{}, fmt.Errorf("%w: %w", domain.ErrInvalidMoney, domain.ErrOverflow)
		}
		return Money{}, fmt.Errorf("%w: invalid whole amount: %v", domain.ErrInvalidMoney, err)
	}
	fraction, err := strconv.ParseUint(fractionPart, 10, 8)
	if err != nil {
		return Money{}, fmt.Errorf("%w: invalid fractional amount: %v", domain.ErrInvalidMoney, err)
	}

	const maxNegativeMagnitude = uint64(math.MaxInt64) + 1
	maxMagnitude := uint64(math.MaxInt64)
	if negative {
		maxMagnitude = maxNegativeMagnitude
	}
	if whole > (maxMagnitude-fraction)/100 {
		return Money{}, fmt.Errorf("%w: %w", domain.ErrInvalidMoney, domain.ErrOverflow)
	}

	magnitude := whole*100 + fraction
	var minor int64
	switch {
	case !negative:
		minor = int64(magnitude)
	case magnitude == maxNegativeMagnitude:
		minor = math.MinInt64
	default:
		minor = -int64(magnitude)
	}

	return NewMinor(minor, currency)
}

func ParseNonNegativeAmount(amount string, currency Currency) (Money, error) {
	value, err := ParseAmount(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if value.minor < 0 {
		return Money{}, fmt.Errorf("%w: negative external amount", domain.ErrInvalidMoney)
	}
	return value, nil
}

func (m Money) IsValid() bool {
	return m.valid && validCurrency(m.currency)
}

func (m Money) MinorUnits() (int64, error) {
	if !m.IsValid() {
		return 0, domain.ErrInvalidMoney
	}
	return m.minor, nil
}

func (m Money) Currency() (Currency, error) {
	if !m.IsValid() {
		return "", domain.ErrInvalidMoney
	}
	return m.currency, nil
}

func (m Money) IsZero() bool {
	return m.IsValid() && m.minor == 0
}

func (m Money) IsPositive() bool {
	return m.IsValid() && m.minor > 0
}

func (m Money) IsNegative() bool {
	return m.IsValid() && m.minor < 0
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor > math.MaxInt64-other.minor) ||
		(other.minor < 0 && m.minor < math.MinInt64-other.minor) {
		return Money{}, domain.ErrOverflow
	}
	return NewMinor(m.minor+other.minor, m.currency)
}

func (m Money) Sub(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor < 0 && m.minor > math.MaxInt64+other.minor) ||
		(other.minor > 0 && m.minor < math.MinInt64+other.minor) {
		return Money{}, domain.ErrOverflow
	}
	return NewMinor(m.minor-other.minor, m.currency)
}

func (m Money) Negate() (Money, error) {
	if !m.IsValid() {
		return Money{}, domain.ErrInvalidMoney
	}
	if m.minor == math.MinInt64 {
		return Money{}, domain.ErrOverflow
	}
	return NewMinor(-m.minor, m.currency)
}

func (m Money) Compare(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid>"
	}
	if m.minor == math.MinInt64 {
		return "-92233720368547758.08"
	}
	negative := m.minor < 0
	magnitude := uint64(m.minor)
	if negative {
		magnitude = uint64(-m.minor)
	}
	sign := ""
	if negative {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d", sign, magnitude/100, magnitude%100)
}

func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, domain.ErrInvalidMoney
	}
	return json.Marshal(struct {
		Amount   string   `json:"amount"`
		Currency Currency `json:"currency"`
	}{Amount: m.String(), Currency: m.currency})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var input struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrInvalidMoney, err)
	}
	currency, err := NewCurrency(input.Currency)
	if err != nil {
		return err
	}
	parsed, err := ParseAmount(input.Amount, currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func (m Money) compatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return domain.ErrInvalidMoney
	}
	if m.currency != other.currency {
		return domain.ErrCurrencyMismatch
	}
	return nil
}

func validCurrency(currency Currency) bool {
	switch currency {
	case BRL, USD, EUR:
		return true
	default:
		return false
	}
}
