package store_test

import (
	"os"
	"testing"

	"github.com/dellarb/mailmoose/internal/auth"
)

func TestMain(m *testing.M) {
	auth.SetIterationsForTest(1000)
	os.Exit(m.Run())
}
