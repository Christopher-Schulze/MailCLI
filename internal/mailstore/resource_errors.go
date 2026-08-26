package mailstore

import (
	"errors"
	"fmt"
	"io"
)

func joinCloseError(resultErr *error, resource io.Closer, description string) {
	if err := resource.Close(); err != nil {
		*resultErr = errors.Join(*resultErr, fmt.Errorf("close %s: %w", description, err))
	}
}
