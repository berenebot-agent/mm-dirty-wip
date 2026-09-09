package httpapp_test

import (
	"os"
	"testing"

	"gatehouse-mail/internal/auth"
)

func TestMain(m *testing.M) {
	auth.SetIterationsForTest(1000)
	os.Exit(m.Run())
}
