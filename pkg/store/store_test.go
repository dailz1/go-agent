package store

import "testing"

func TestMemoryStoreContract(t *testing.T) {
	t.Parallel()
	exerciseContract(t, func() Store { return NewMemory() })
	exerciseRetryContract(t, func() Store { return NewMemory() })
}
