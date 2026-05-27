package sqlite_test

import (
	"testing"
)

func TestProbeFTS5_Success(t *testing.T) {
	s := newTestStore(t)
	if s.DB() == nil {
		t.Error("DB() should not return nil")
	}
}
