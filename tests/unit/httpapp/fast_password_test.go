package httpapp_test

import (
	"os"
	"testing"

	"github.com/dellarb/mailmoose/internal/auth"
)

func TestMain(m *testing.M) {
	auth.SetArgonParamsForTest(8, 1)
	os.Exit(m.Run())
}
