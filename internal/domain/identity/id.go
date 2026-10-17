package identity

import (
	"fmt"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
)

func Validate(value string) error {
	if len(value) != 36 {
		return fmt.Errorf("%w: UUID must contain 36 characters", domain.ErrInvalidID)
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return domain.ErrInvalidID
			}
		default:
			char := value[i]
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return domain.ErrInvalidID
			}
		}
	}
	return nil
}
